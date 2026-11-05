// pespy —— PE / 数据文件静态侦察工具（Go 版，零第三方依赖）。
//
// 复刻此前用 Python(pefile) 对 WeChatWin.dll 完成的全部静态侦察能力：
//   - PE 头画像（架构 / 子系统 / 链接器 / 编译时间戳 / 版本资源）
//   - 区段表 + Shannon 熵（加壳启发判断）
//   - 导入表（DLL 清单 + 每 DLL 函数计数）
//   - 导出表（IMAGE_EXPORT_DIRECTORY 手工解析，debug/pe 不提供导出 API）
//   - RTTI 类型描述符扫描（MSVC .?AV / .?AU mangled name）
//   - 目录递归扫描（magic 头识别 + 熵摘要，用于加密数据库 / 未知格式探测）
//
// 纯标准库（debug/pe + encoding/binary 手工 RVA 解析），零第三方依赖。
//
// 构建：go build -o pespy.exe ./cmd/pespy（产物不入版本库，.gitignore 已忽略）。
//
// 用法:
//
//	pespy.exe <file.dll|file.exe> [更多文件...]   // 深度分析
//	pespy.exe <目录>                              // 递归扫描，输出 magic/熵摘要
package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

func main() {
	// Windows 控制台切 UTF-8 输出，避免 chcp 936 下中文乱码；失败忽略（非 Windows 场景）
	syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleOutputCP").Call(65001)

	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Println("用法: pespy <文件|目录> [更多路径...]")
		fmt.Println("  文件: PE 则深度画像（头/区段熵/导入/导出/RTTI/版本资源），否则输出 magic+熵")
		fmt.Println("  目录: 递归扫描，每文件一行 magic/熵摘要")
		os.Exit(2)
	}
	for _, arg := range args {
		st, err := os.Stat(arg)
		if err != nil {
			fmt.Printf("!! %s: %v\n", arg, err)
			continue
		}
		if st.IsDir() {
			scanDir(arg)
			continue
		}
		analyzeFile(arg)
	}
}

// ---------- 文件级入口 ----------

func analyzeFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("!! 读取失败 %s: %v\n", path, err)
		return
	}
	if len(data) >= 2 && data[0] == 'M' && data[1] == 'Z' {
		analyzePE(path, data)
		return
	}
	analyzeGeneric(path, data)
}

// analyzeGeneric 非 PE 文件：magic 头 + 熵，识别 SQLite 明文库 / SQLCipher 加密库等
func analyzeGeneric(path string, data []byte) {
	fmt.Printf("== %s ==\n", path)
	fmt.Printf("  大小   : %s\n", humanSize(int64(len(data))))
	magic, label := sniffMagic(data)
	fmt.Printf("  Magic  : %s\n", magic)
	fmt.Printf("  判定   : %s（整文件熵 %.2f）\n", label, shannon(data))
	// SQLite 明文库特征头 16 字节；加密库则为随机字节
	fmt.Printf("  头16B  : % x\n", data[:min(16, len(data))])
	fmt.Println()
}

// sniffMagic 已知文件头识别
func sniffMagic(data []byte) (hexHead, label string) {
	head := data[:min(16, len(data))]
	hexHead = fmt.Sprintf("% x", head)
	switch {
	case bytes.HasPrefix(data, []byte("SQLite format 3\x00")):
		return hexHead, "SQLite 明文数据库（未加密）"
	case len(data) > 64 && shannon(data[:min(len(data), 1<<20)]) > 7.5:
		return hexHead, "高熵数据（疑似加密/压缩，SQLCipher 类库文件特征）"
	default:
		return hexHead, "未知格式"
	}
}

// ---------- PE 深度分析 ----------

func analyzePE(path string, data []byte) {
	fmt.Printf("== %s ==\n", path)
	fmt.Printf("  大小      : %s\n", humanSize(int64(len(data))))

	f, err := pe.NewFile(bytes.NewReader(data))
	if err != nil {
		fmt.Printf("  PE 解析   : 失败（%v）\n\n", err)
		return
	}
	defer f.Close()

	// --- 基础头 ---
	subsys, linker, numDD := uint16(0), "", 0
	var exportDD, importDD, relocDD pe.DataDirectory
	switch oh := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		subsys, numDD = oh.Subsystem, int(oh.NumberOfRvaAndSizes)
		linker = fmt.Sprintf("%d.%02d", oh.MajorLinkerVersion, oh.MinorLinkerVersion)
		if numDD > 6 {
			exportDD, importDD, relocDD = oh.DataDirectory[0], oh.DataDirectory[1], oh.DataDirectory[5]
		}
	case *pe.OptionalHeader32:
		subsys, numDD = oh.Subsystem, int(oh.NumberOfRvaAndSizes)
		linker = fmt.Sprintf("%d.%02d", oh.MajorLinkerVersion, oh.MinorLinkerVersion)
		if numDD > 6 {
			exportDD, importDD, relocDD = oh.DataDirectory[0], oh.DataDirectory[1], oh.DataDirectory[5]
		}
	}
	fmt.Printf("  架构      : %s   子系统: %s   链接器: %s\n",
		machineName(f.FileHeader.Machine), subsysName(subsys), linker)
	fmt.Printf("  编译时间  : %s（PE TimeDateStamp）\n", time.Unix(int64(f.FileHeader.TimeDateStamp), 0).UTC().Format("2006-01-02 15:04:05 UTC"))

	// --- 版本资源（走 Win32 version.dll API，比手工解析资源节可靠） ---
	vi := versionInfo(path)
	if len(vi) > 0 {
		keys := []string{"CompanyName", "ProductVersion", "FileVersion", "FileDescription"}
		var parts []string
		for _, k := range keys {
			if v := vi[k]; v != "" {
				parts = append(parts, k+"="+v)
			}
		}
		fmt.Printf("  版本资源  : %s\n", strings.Join(parts, "  "))
	}

	// --- 区段 + 熵 ---
	type secInfo struct {
		name       string
		vsize      uint32
		rsize      uint32
		entropy    float64
		exec, writ bool
	}
	secs := make([]secInfo, 0, len(f.Sections))
	suspect := false
	for _, s := range f.Sections {
		e := 0.0
		if d, err := s.Data(); err == nil && len(d) > 0 {
			e = shannon(d)
		}
		ch := s.Characteristics
		si := secInfo{strings.TrimRight(s.Name, "\x00"), s.VirtualSize, s.Size, e,
			ch&pe.IMAGE_SCN_MEM_EXECUTE != 0, ch&pe.IMAGE_SCN_MEM_WRITE != 0}
		secs = append(secs, si)
		// 加壳启发：高熵可执行节是压缩/加密壳的强信号，但排除腾讯 IPP 动态生成节（.Dl1 类；
		// 正常 MSVC .text ≈ 6.0~6.8，运行时填充的段落名无点前缀或非常规名需结合导入表判断）
		if si.exec && e > 7.2 && si.name != ".Dl1" {
			suspect = true
		}
	}
	var secLines []string
	for _, si := range secs {
		flag := ""
		if si.exec {
			flag += "X"
		}
		if si.writ {
			flag += "W"
		}
		secLines = append(secLines, fmt.Sprintf("%s[%s %.1f]", si.name, flag, si.entropy))
	}
	fmt.Printf("  区段(%d)   : %s\n", len(secs), strings.Join(secLines, "  "))

	// --- 导入表（手工遍历 IMAGE_IMPORT_DESCRIPTOR；debug/pe 的 ImportedSymbols 对该 DLL 解析失真） ---
	dlls, totalFuncs := parseImports(data, f, importDD)
	totalDLLs := len(dlls)
	impParts := make([]string, 0, totalDLLs)
	for _, d := range dlls {
		impParts = append(impParts, fmt.Sprintf("%s(%d)", d.name, len(d.funcs)))
	}
	if len(impParts) > 40 { // DLL 多时截断展示，完整清单可加 -imports 开关
		impParts = impParts[:40]
		impParts = append(impParts, "…")
	}
	fmt.Printf("  导入(%d DLL / %d 函数): %s\n", totalDLLs, totalFuncs, strings.Join(impParts, " "))

	// --- 导出表（手工解析 IMAGE_EXPORT_DIRECTORY） ---
	exports := parseExports(data, f, exportDD)
	if len(exports) > 0 {
		show := exports
		if len(show) > 60 {
			show = show[:60]
		}
		fmt.Printf("  导出(%d)   : %s\n", len(exports), strings.Join(show, ", "))
	} else if exportDD.VirtualAddress != 0 {
		fmt.Println("  导出      : 目录存在但解析失败")
	} else {
		fmt.Println("  导出      : 无（0 个）")
	}

	// --- RTTI 类型描述符 ---
	classes, rttiTotal := scanRTTI(data)
	if rttiTotal > 0 {
		fmt.Printf("  RTTI      : 检出 %d 个类描述符，如: %s\n", rttiTotal, strings.Join(classes, ", "))
	} else {
		fmt.Println("  RTTI      : 未检出（可能关闭 /RTTI 或被混淆）")
	}

	// --- 加壳/混淆启发结论 ---
	conclusion := "未检出壳/混淆（节熵正常、导入表为标准 DLL）"
	if suspect {
		conclusion = "疑似加壳/加密：存在高熵可执行节"
	}
	if totalDLLs > 0 && totalDLLs < 3 {
		conclusion = "疑似加壳/压缩：导入 DLL 数量异常少（" + fmt.Sprint(totalDLLs) + " 个）"
	}
	if relocDD.VirtualAddress == 0 && f.FileHeader.Machine == pe.IMAGE_FILE_MACHINE_AMD64 {
		conclusion += "；.reloc 缺失（异常）"
	}
	fmt.Printf("  结论      : %s\n\n", conclusion)
}

// parseExports 手工解析导出表：RVA → 文件偏移 → IMAGE_EXPORT_DIRECTORY → 名称数组
func parseExports(data []byte, f *pe.File, dd pe.DataDirectory) []string {
	if dd.VirtualAddress == 0 {
		return nil
	}
	off, ok := rvaToOff(f, dd.VirtualAddress)
	if !ok || int(off)+40 > len(data) {
		return nil
	}
	nNames := binary.LittleEndian.Uint32(data[off+24:])
	namesRVA := binary.LittleEndian.Uint32(data[off+32:])
	if nNames == 0 || namesRVA == 0 {
		return nil
	}
	nOff, ok := rvaToOff(f, namesRVA)
	if !ok {
		return nil
	}
	out := make([]string, 0, nNames)
	for i := uint32(0); i < nNames; i++ {
		if int(nOff+i*4)+4 > len(data) {
			break
		}
		nRVA := binary.LittleEndian.Uint32(data[nOff+i*4:])
		sOff, ok := rvaToOff(f, nRVA)
		if !ok {
			out = append(out, fmt.Sprintf("<rva 0x%X>", nRVA))
			continue
		}
		out = append(out, cstr(data, sOff))
	}
	return out
}

// impDLL 单个导入 DLL 及其函数名
type impDLL struct {
	name  string
	funcs []string
}

// parseImports 手工遍历 IMAGE_IMPORT_DESCRIPTOR 数组（以全零 20 字节条目结尾）。
// OriginalFirstThunk 优先（保留原始序），为 0 时退回 FirstThunk；
// x64 thunk 表项 8 字节，最高位 1 = 按序号导入，否则指向 IMAGE_IMPORT_BY_NAME（前 2 字节 Hint + 函数名）
func parseImports(data []byte, f *pe.File, dd pe.DataDirectory) ([]impDLL, int) {
	if dd.VirtualAddress == 0 {
		return nil, 0
	}
	off, ok := rvaToOff(f, dd.VirtualAddress)
	if !ok {
		return nil, 0
	}
	var out []impDLL
	total := 0
	for int(off)+20 <= len(data) {
		oft := binary.LittleEndian.Uint32(data[off:])
		nameRVA := binary.LittleEndian.Uint32(data[off+12:])
		if nameRVA == 0 {
			break // 终止条目
		}
		var d impDLL
		if nOff, ok := rvaToOff(f, nameRVA); ok {
			d.name = cstr(data, nOff)
		}
		thunkRVA := oft
		if thunkRVA == 0 {
			thunkRVA = binary.LittleEndian.Uint32(data[off+16:])
		}
		if tOff, ok := rvaToOff(f, thunkRVA); ok {
			for int(tOff)+8 <= len(data) {
				v := binary.LittleEndian.Uint64(data[tOff:])
				if v == 0 {
					break
				}
				if v&(1<<63) != 0 { // 按序号导入
					d.funcs = append(d.funcs, fmt.Sprintf("#%d", v&0xFFFF))
				} else if hOff, ok := rvaToOff(f, uint32(v)); ok {
					d.funcs = append(d.funcs, cstr(data, hOff+2)) // 跳过 2 字节 Hint
				}
				tOff += 8
			}
		}
		if d.name != "" {
			out = append(out, d)
			total += len(d.funcs)
		}
		off += 20
	}
	return out, total
}

// rvaToOff 区段映射：RVA → 文件内偏移
func rvaToOff(f *pe.File, rva uint32) (uint32, bool) {
	for _, s := range f.Sections {
		vmax := s.VirtualSize
		if s.Size > vmax {
			vmax = s.Size
		}
		if s.VirtualAddress <= rva && rva < s.VirtualAddress+vmax {
			delta := rva - s.VirtualAddress
			if delta >= s.Size {
				return 0, false // 落在虚拟尾部（仅内存零填充，文件中不存在）
			}
			return s.Offset + delta, true
		}
	}
	return 0, false
}

func cstr(data []byte, off uint32) string {
	if int(off) >= len(data) {
		return ""
	}
	end := bytes.IndexByte(data[off:], 0)
	if end < 0 {
		return string(data[off:])
	}
	return string(data[off : uint32(end)+off])
}

// scanRTTI 搜 MSVC mangled 类型描述符：.?AV<ClassName@Namespace>@@ / .?AU<Struct>@
// 返回（前 15 个可读名, 总数）
func scanRTTI(data []byte) ([]string, int) {
	var out []string
	total := 0
	for _, sig := range []string{".?AV", ".?AU"} {
		pat := []byte(sig)
		from := 0
		for {
			i := bytes.Index(data[from:], pat)
			if i < 0 {
				break
			}
			start := from + i + len(sig)
			total++
			if end := bytes.Index(data[start:start+min(256, len(data)-start)], []byte("@@")); end > 0 {
				if len(out) < 15 {
					out = append(out, sig[:3]+string(data[start:start+end])) // 去掉 @@ 只留类名
				}
			}
			from = start
		}
	}
	return out, total
}

// ---------- 目录扫描 ----------

func scanDir(dir string) {
	fmt.Printf("== [目录扫描] %s ==\n", dir)
	var files []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil // 权限错误等静默跳过，不中断扫描
		}
		files = append(files, p)
		return nil
	})
	fmt.Printf("  共 %d 个文件\n", len(files))
	for _, p := range files {
		info, _ := os.Stat(p)
		h, err := os.Open(p)
		if err != nil {
			fmt.Printf("  %-58s %10s  <打开失败>\n", rel(dir, p), humanSize(info.Size()))
			continue
		}
		head := make([]byte, 64<<10) // 头 64KB 采样算熵，避免大文件全读
		n, _ := h.Read(head)
		h.Close()
		head = head[:n]
		var label string
		switch {
		case bytes.HasPrefix(head, []byte("MZ")):
			label = "PE"
		case bytes.HasPrefix(head, []byte("SQLite format 3\x00")):
			label = "SQLite明文库"
		case shannon(head) > 7.5 && n >= 4096:
			label = "高熵(加密/压缩?)"
		default:
			label = "数据"
		}
		fmt.Printf("  %-58s %10s  熵%.2f  %s\n", rel(dir, p), humanSize(info.Size()), shannon(head), label)
	}
	fmt.Println()
}

// ---------- 工具函数 ----------

// shannon Shannon 熵（bits/byte，满值 8.0）
func shannon(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var hist [256]int
	for _, x := range b {
		hist[x]++
	}
	e := 0.0
	n := float64(len(b))
	for _, c := range hist {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		e -= p * math.Log2(p)
	}
	return e
}

// versionInfo 读取 PE 版本资源（VS_VERSIONINFO），走 Win32 version.dll API，可靠且支持任意路径
func versionInfo(path string) map[string]string {
	out := map[string]string{}
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return out
	}
	r1, _, _ := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(pathPtr)), 0) // 2 参签名，末位 lpdwHandle
	if r1 == 0 {
		return out
	}
	size := r1
	buf := make([]byte, size)
	r2, _, _ := procGetFileVersionInfoW.Call(uintptr(unsafe.Pointer(pathPtr)), 0, size, uintptr(unsafe.Pointer(&buf[0])))
	if r2 == 0 {
		return out
	}
	// 先查翻译表拿第一个语言代码页，再读各字符串键值
	// 注意：VerQueryValueW 的出参是指向 buf 内部的指针，需解引用才能拿到 DWORD 值
	var translatePtr *uint32
	var tLen uint32
	r3, _, _ := procVerQueryValueW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&subBlockTranslate[0])),
		uintptr(unsafe.Pointer(&translatePtr)), uintptr(unsafe.Pointer(&tLen)))
	if r3 == 0 || tLen < 4 || translatePtr == nil {
		return out
	}
	translate := *translatePtr
	for _, key := range []string{"CompanyName", "FileDescription", "FileVersion", "ProductName", "ProductVersion", "LegalCopyright"} {
		path16 := utf16.Encode([]rune(fmt.Sprintf("\\StringFileInfo\\%04x%04x\\%s", translate&0xFFFF, translate>>16, key)))
		path16 = append(path16, 0)
		var valPtr *uint16
		var valLen uint32
		r4, _, _ := procVerQueryValueW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&path16[0])),
			uintptr(unsafe.Pointer(&valPtr)), uintptr(unsafe.Pointer(&valLen)))
		if r4 != 0 && valLen > 0 && valPtr != nil {
			out[key] = strings.TrimRight(string(utf16.Decode(unsafe.Slice(valPtr, valLen))), "\x00")
		}
	}
	return out
}

// Win32 version.dll 绑定（仅 Windows；syscall.NewLazyDLL 失败时进程无法启动，但本工具本就是 Windows 工具）
var (
	modversion                  = syscall.NewLazyDLL("version.dll")
	procGetFileVersionInfoSizeW = modversion.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = modversion.NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = modversion.NewProc("VerQueryValueW")
	subBlockTranslate           = []uint16{'\\', 'V', 'a', 'r', 'F', 'i', 'l', 'e', 'I', 'n', 'f', 'o', '\\', 'T', 'r', 'a', 'n', 's', 'l', 'a', 't', 'i', 'o', 'n', 0}
)

func machineName(m uint16) string {
	switch m {
	case pe.IMAGE_FILE_MACHINE_AMD64:
		return "x64 (AMD64)"
	case pe.IMAGE_FILE_MACHINE_I386:
		return "x86 (i386)"
	case pe.IMAGE_FILE_MACHINE_ARM64:
		return "ARM64"
	case pe.IMAGE_FILE_MACHINE_ARMNT:
		return "ARM Thumb-2"
	default:
		return fmt.Sprintf("0x%04X", m)
	}
}

func subsysName(s uint16) string {
	switch s {
	case 2:
		return "GUI"
	case 3:
		return "Console"
	case 1:
		return "Native"
	default:
		return fmt.Sprintf("%d", s)
	}
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func rel(dir, p string) string {
	if r, err := filepath.Rel(dir, p); err == nil {
		return r
	}
	return p
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

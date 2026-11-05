package main

// 回归测试：OOXML 读写往返 + pptx 结构合规（zip 条目 / Content_Types / rels /
// presentation sldMasterIdLst / 每页独立 p:sp（各 1 个 nvSpPr + 1 个 txBody）/
// 图片嵌入（media + p:pic + image rel + ContentTypes Default））。
import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlideRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deck.pptx")
	m := &slideModel{Pages: []slidePage{
		{ID: "s1", Title: "封面", Lines: []string{"你好", "世界"}},
		{ID: "s2", Title: "第二页", Lines: []string{"内容", "第二行"}},
		{ID: "s3", Title: "第三页", Lines: []string{"结束"}},
	}}
	if err := writeSlide(path, m); err != nil {
		t.Fatalf("writeSlide: %v", err)
	}
	got, err := readSlide(path)
	if err != nil {
		t.Fatalf("readSlide: %v", err)
	}
	if len(got.Pages) != 3 {
		t.Fatalf("pages = %d, want 3", len(got.Pages))
	}
	if got.Pages[0].Title != "封面" || len(got.Pages[0].Lines) != 2 || got.Pages[0].Lines[0] != "你好" {
		t.Fatalf("page1 mismatch: %+v", got.Pages[0])
	}
	if got.Pages[2].Title != "第三页" || got.Pages[2].Lines[0] != "结束" {
		t.Fatalf("page3 mismatch: %+v", got.Pages[2])
	}
}

func TestDocxRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.docx")
	m := &docModel{Title: "标题", Paras: []string{"第一段", "第二段"}}
	if err := writeDocx(path, m); err != nil {
		t.Fatalf("writeDocx: %v", err)
	}
	got, err := readDocx(path)
	if err != nil {
		t.Fatalf("readDocx: %v", err)
	}
	if len(got.Paras) != 2 || got.Paras[0] != "第一段" || got.Paras[1] != "第二段" {
		t.Fatalf("paras mismatch: %+v", got.Paras)
	}
}

// zipEntries 返回 zip 包条目名集合。
func zipEntries(t *testing.T, path string) map[string]bool {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	out := map[string]bool{}
	for _, f := range zr.File {
		out[f.Name] = true
	}
	return out
}

// readEntry 读取 zip 包内条目文本。
func readEntry(t *testing.T, path, name string) string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open %s: %v", name, err)
			}
			defer rc.Close()
			var sb strings.Builder
			if _, err := io.Copy(&sb, rc); err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			return sb.String()
		}
	}
	t.Fatalf("zip entry %q missing", name)
	return ""
}

// readEntryBytes 读取 zip 包内二进制条目。
func readEntryBytes(t *testing.T, path, name string) []byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open %s: %v", name, err)
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			return b
		}
	}
	t.Fatalf("zip entry %q missing", name)
	return nil
}

// makePNG 生成一张纯色小 PNG（测试图片数据）。
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 0x44, G: 0x72, B: 0xC4, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// TestPptxStructureComplete 核心回归：多页 pptx 的包结构与关键 part 齐全且合规。
func TestPptxStructureComplete(t *testing.T) {
	const pages = 3
	path := filepath.Join(t.TempDir(), "deck.pptx")
	m := &slideModel{Pages: []slidePage{
		{ID: "s1", Title: "封面", Lines: []string{"你好", "世界"}},
		{ID: "s2", Title: "第二页", Lines: []string{"内容"}},
		{ID: "s3", Title: "第三页", Lines: []string{"结束"}},
	}}
	if err := writeSlide(path, m); err != nil {
		t.Fatalf("writeSlide: %v", err)
	}

	// 1) zip 内必须包含全部关键条目。
	names := zipEntries(t, path)
	required := []string{
		"[Content_Types].xml",
		"_rels/.rels",
		"docProps/core.xml",
		"docProps/app.xml",
		"ppt/presentation.xml",
		"ppt/_rels/presentation.xml.rels",
		"ppt/presProps.xml",
		"ppt/viewProps.xml",
		"ppt/tableStyles.xml",
		"ppt/theme/theme1.xml",
		"ppt/slideMasters/slideMaster1.xml",
		"ppt/slideMasters/_rels/slideMaster1.xml.rels",
		"ppt/slideLayouts/slideLayout1.xml",
		"ppt/slideLayouts/_rels/slideLayout1.xml.rels",
	}
	for i := 1; i <= pages; i++ {
		required = append(required,
			"ppt/slides/slide"+itoa(i)+".xml",
			"ppt/slides/_rels/slide"+itoa(i)+".xml.rels")
	}
	for _, r := range required {
		if !names[r] {
			t.Errorf("missing zip entry: %s", r)
		}
	}

	// 2) Content_Types 覆盖全部 slide + master/layout/theme/presProps/viewProps/tableStyles。
	ct := readEntry(t, path, "[Content_Types].xml")
	for _, want := range []string{
		`PartName="/ppt/presentation.xml"`,
		`PartName="/ppt/slideMasters/slideMaster1.xml"`,
		`PartName="/ppt/slideLayouts/slideLayout1.xml"`,
		`PartName="/ppt/theme/theme1.xml"`,
		`PartName="/ppt/presProps.xml"`,
		`PartName="/ppt/viewProps.xml"`,
		`PartName="/ppt/tableStyles.xml"`,
		`PartName="/docProps/core.xml"`,
		`PartName="/docProps/app.xml"`,
	} {
		if !strings.Contains(ct, want) {
			t.Errorf("Content_Types missing %s", want)
		}
	}
	for i := 1; i <= pages; i++ {
		if !strings.Contains(ct, `PartName="/ppt/slides/slide`+itoa(i)+`.xml"`) {
			t.Errorf("Content_Types missing Override for slide%d.xml", i)
		}
	}

	// 3) presentation.xml 含 sldMasterIdLst / 唯一 sldId / sldSz 16:9。
	pres := readEntry(t, path, "ppt/presentation.xml")
	for _, want := range []string{"sldMasterIdLst", `id="2147483648"`, "sldIdLst", `cx="12192000" cy="6858000"`} {
		if !strings.Contains(pres, want) {
			t.Errorf("presentation.xml missing %q", want)
		}
	}
	if strings.Count(pres, "<p:sldId ") != pages {
		t.Errorf("presentation.xml sldId count = %d, want %d", strings.Count(pres, "<p:sldId "), pages)
	}

	// 4) presentation.xml.rels 含 slideMaster/theme/presProps/viewProps/tableStyles + 全部 slide。
	presRels := readEntry(t, path, "ppt/_rels/presentation.xml.rels")
	for _, want := range []string{
		`Type="` + nsDocRel + `/slideMaster" Target="slideMasters/slideMaster1.xml"`,
		`Type="` + nsDocRel + `/theme" Target="theme/theme1.xml"`,
		`Type="` + nsDocRel + `/presProps" Target="presProps.xml"`,
		`Type="` + nsDocRel + `/viewProps" Target="viewProps.xml"`,
		`Type="` + nsDocRel + `/tableStyles" Target="tableStyles.xml"`,
	} {
		if !strings.Contains(presRels, want) {
			t.Errorf("presentation.xml.rels missing %s", want)
		}
	}
	for i := 1; i <= pages; i++ {
		if !strings.Contains(presRels, `Target="slides/slide`+itoa(i)+`.xml"`) {
			t.Errorf("presentation.xml.rels missing slide%d.xml", i)
		}
	}

	// 5) 每个 slideN.xml.rels 必须引用 slideLayout1。
	for i := 1; i <= pages; i++ {
		sr := readEntry(t, path, "ppt/slides/_rels/slide"+itoa(i)+".xml.rels")
		if !strings.Contains(sr, `../slideLayouts/slideLayout1.xml`) || !strings.Contains(sr, `/slideLayout"`) {
			t.Errorf("slide%d.xml.rels missing slideLayout reference", i)
		}
	}

	// 6) 每个 slideN.xml：标题/正文为两个独立 <p:sp>，各含恰好 1 个 nvSpPr + 1 个 txBody。
	for i := 1; i <= pages; i++ {
		sl := readEntry(t, path, "ppt/slides/slide"+itoa(i)+".xml")
		parts := strings.Split(sl, "<p:sp>")
		if len(parts)-1 != 2 {
			t.Errorf("slide%d.xml p:sp count = %d, want 2 (title + content)", i, len(parts)-1)
		}
		for j, frag := range parts[1:] {
			if got := strings.Count(frag, "<p:nvSpPr>"); got != 1 {
				t.Errorf("slide%d.xml sp#%d nvSpPr count = %d, want 1", i, j+1, got)
			}
			if got := strings.Count(frag, "<p:txBody>"); got != 1 {
				t.Errorf("slide%d.xml sp#%d txBody count = %d, want 1", i, j+1, got)
			}
		}
		if !strings.Contains(sl, `>封面</a:t>`) && i == 1 {
			t.Errorf("slide1.xml title text missing")
		}
	}

	// 7) master/layout/theme 自引用关系。
	master := readEntry(t, path, "ppt/slideMasters/slideMaster1.xml")
	for _, want := range []string{"<p:cSld>", "<p:spTree>", "sldLayoutIdLst", "<p:txStyles>"} {
		if !strings.Contains(master, want) {
			t.Errorf("slideMaster1.xml missing %q", want)
		}
	}
	if !strings.Contains(readEntry(t, path, "ppt/slideMasters/_rels/slideMaster1.xml.rels"), "../theme/theme1.xml") {
		t.Errorf("slideMaster rels missing theme")
	}
	if !strings.Contains(readEntry(t, path, "ppt/slideLayouts/_rels/slideLayout1.xml.rels"), "../slideMasters/slideMaster1.xml") {
		t.Errorf("slideLayout rels missing master")
	}
	theme := readEntry(t, path, "ppt/theme/theme1.xml")
	for _, want := range []string{"<a:clrScheme", "<a:fontScheme", "<a:fmtScheme", "<a:fillStyleLst", "<a:lnStyleLst", "<a:effectStyleLst", "<a:bgFillStyleLst"} {
		if !strings.Contains(theme, want) {
			t.Errorf("theme1.xml missing %q", want)
		}
	}
}

// TestPptxImageEmbed 图片嵌入回归：media 条目、p:pic、image rel、ContentTypes Default。
func TestPptxImageEmbed(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(imgPath, makePNG(t, 200, 120), 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}
	path := filepath.Join(dir, "deck.pptx")
	t.Setenv("GOFFICE_WORKSPACE", dir)
	m := &slideModel{Pages: []slidePage{
		{ID: "s1", Title: "封面", Lines: []string{"你好"}, ImagePath: "pic.png"},
		{ID: "s2", Title: "第二页", Lines: []string{"内容"}},
	}}
	if err := writeSlide(path, m); err != nil {
		t.Fatalf("writeSlide: %v", err)
	}
	names := zipEntries(t, path)
	if !names["ppt/media/image1.png"] {
		t.Errorf("missing ppt/media/image1.png")
	}
	// media 内容须与源 PNG 一致。
	srcBytes, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatalf("read source png: %v", err)
	}
	if !bytes.Equal(readEntryBytes(t, path, "ppt/media/image1.png"), srcBytes) {
		t.Errorf("media image bytes mismatch")
	}
	// slide1 含 <p:pic> 且 r:embed 指向 rId2；slide2 无 pic。
	sl1 := readEntry(t, path, "ppt/slides/slide1.xml")
	if !strings.Contains(sl1, "<p:pic>") || !strings.Contains(sl1, `r:embed="rId2"`) {
		t.Errorf("slide1 missing p:pic/rId2")
	}
	sl2 := readEntry(t, path, "ppt/slides/slide2.xml")
	if strings.Contains(sl2, "<p:pic>") {
		t.Errorf("slide2 should not contain p:pic")
	}
	// slide1 rels 含 image 关系到 ../media/image1.png。
	rel1 := readEntry(t, path, "ppt/slides/_rels/slide1.xml.rels")
	if !strings.Contains(rel1, `Target="../media/image1.png"`) || !strings.Contains(rel1, `/image"`) {
		t.Errorf("slide1 rels missing image relationship")
	}
	// Content_Types 含 png Default。
	ct := readEntry(t, path, "[Content_Types].xml")
	if !strings.Contains(ct, `Extension="png" ContentType="image/png"`) {
		t.Errorf("Content_Types missing png default")
	}
}

// TestPptxVerifyArtifact 生成固定产物到 GOFFICE_TEST_ARTIFACT 指定路径（供外部
// python-pptx 独立打开验证；未设置环境变量时跳过）。
func TestPptxVerifyArtifact(t *testing.T) {
	out := os.Getenv("GOFFICE_TEST_ARTIFACT")
	if out == "" {
		t.Skip("GOFFICE_TEST_ARTIFACT not set")
	}
	m := &slideModel{Pages: []slidePage{
		{ID: "s1", Title: "封面", Lines: []string{"你好，世界", "第二行内容"}},
		{ID: "s2", Title: "第二页", Lines: []string{"内容A", "内容B"}},
		{ID: "s3", Title: "Third Page", Lines: []string{"End"}},
	}}
	if err := writeSlide(out, m); err != nil {
		t.Fatalf("writeSlide: %v", err)
	}
	t.Logf("artifact written: %s", out)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

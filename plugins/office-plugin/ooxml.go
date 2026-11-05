// docx / pptx 最小 OOXML 引擎（标准库 archive/zip + encoding/xml）。
//
// 目标：创建/读取可被 Word / WPS / PowerPoint 打开的合法 docx / pptx，
// 支持段落文本（docx）与页面文本框（pptx），以及 pptx 每页可选嵌入
// 图片（png/jpeg/gif）。富格式（表格、样式）不在当前范围。
package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ---------- OOXML 常用命名空间 ----------
const (
	nsWord    = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	nsPkgRel  = "http://schemas.openxmlformats.org/package/2006/relationships"
	nsDocRel  = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	nsContent = "http://schemas.openxmlformats.org/package/2006/content-types"
	nsPML     = "http://schemas.openxmlformats.org/presentationml/2006/main"
	nsDrawing = "http://schemas.openxmlformats.org/drawingml/2006/main"
)

// ---------- docx ----------

// docxDoc 文档根。
type docxDoc struct {
	XMLName xml.Name `xml:"document"`
	Body    docxBody `xml:"body"`
}

type docxBody struct {
	Paras []docxPara `xml:"p"`
}

type docxPara struct {
	Runs []docxRun `xml:"r"`
}

type docxRun struct {
	Text *docxText `xml:"t,omitempty"`
}

type docxText struct {
	Text string `xml:",chardata"`
}

// writeDocx 生成最小 docx（标题 + 段落）。
func writeDocx(path string, m *docModel) error {
	var body strings.Builder
	if m.Title != "" {
		body.WriteString(`<w:p><w:pPr><w:jc w:val="center"/></w:pPr><w:r><w:rPr><w:b/><w:sz w:val="36"/></w:rPr><w:t xml:space="preserve">` +
			xmlEscape(m.Title) + `</w:t></w:r></w:p>`)
	}
	for _, p := range m.Paras {
		body.WriteString(`<w:p><w:r><w:t xml:space="preserve">` + xmlEscape(p) + `</w:t></w:r></w:p>`)
	}
	document := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="%s"><w:body>%s<w:sectPr><w:pgSz w:w="11906" w:h="16838"/></w:sectPr></w:body></w:document>`,
		nsWord, body.String())
	files := map[string]string{
		"[Content_Types].xml":          `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="` + nsContent + `"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"_rels/.rels":                  `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="` + nsPkgRel + `"><Relationship Id="rId1" Type="` + nsDocRel + `/officeDocument" Target="word/document.xml"/></Relationships>`,
		"word/document.xml":            document,
		"word/_rels/document.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="` + nsPkgRel + `"></Relationships>`,
	}
	return writeZip(path, files)
}

// readDocx 读取 docx 段落文本（<w:p> 内 <w:t> 串接）。
func readDocx(path string) (*docModel, error) {
	documentXML, err := readZipEntry(path, "word/document.xml")
	if err != nil {
		return nil, err
	}
	var doc docxDoc
	if err := xml.Unmarshal(documentXML, &doc); err != nil {
		return nil, fmt.Errorf("parse document.xml: %w", err)
	}
	m := &docModel{}
	for _, p := range doc.Body.Paras {
		var sb strings.Builder
		for _, r := range p.Runs {
			if r.Text != nil {
				sb.WriteString(r.Text.Text)
			}
		}
		if t := strings.TrimSpace(sb.String()); t != "" {
			m.Paras = append(m.Paras, t)
		}
	}
	if len(m.Paras) > 0 {
		m.Title = m.Paras[0]
		m.Paras = m.Paras[1:] // 标题段不计入正文（对齐 writeDocx 的 Title+Paras 模型）
	}
	return m, nil
}

// loadDoc 读取 docx（容错：损坏时返回空模型而非报错，便于后续覆盖）。
func loadDoc(path string) (*docModel, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	m, err := readDocx(path)
	if err != nil {
		// 非 docx 或损坏：返回空模型（能力边界，不冒充）。
		return &docModel{}, nil
	}
	return m, nil
}

// ---------- pptx ----------

// pptxPres 演示文稿根。
type pptxPres struct {
	XMLName    xml.Name       `xml:"presentation"`
	SlideIDLst pptxSlideIDLst `xml:"sldIdLst"`
}

type pptxSlideIDLst struct {
	IDs []pptxSlideID `xml:"sldId"`
}

type pptxSlideID struct {
	ID string `xml:"id,attr"`
}

// pptxSlide 单页根。
type pptxSlide struct {
	XMLName xml.Name `xml:"sld"`
	CSp     pptxCSp  `xml:"cSld"`
}

type pptxCSp struct {
	SpTree pptxSpTree `xml:"spTree"`
}

type pptxSpTree struct {
	Sps  []pptxSp  `xml:"sp"`
	Pics []pptxPic `xml:"pic"`
}

type pptxSp struct {
	TxBody pptxTxBody `xml:"txBody"`
}

type pptxTxBody struct {
	Paras []pptxPara `xml:"p"`
}

type pptxPara struct {
	Runs []pptxRun `xml:"r"`
}

type pptxRun struct {
	Text *pptxText `xml:"t,omitempty"`
}

type pptxText struct {
	Text string `xml:",chardata"`
}

// pptxPic 页内图片（仅取 r:embed 关系 id，用于回填 ImagePath）。
type pptxPic struct {
	BlipFill struct {
		Blip struct {
			Embed string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships embed,attr"`
		} `xml:"blip"`
	} `xml:"blipFill"`
}

// readSlide 读取 pptx 页面文本。
func readSlide(path string) (*slideModel, error) {
	presXML, err := readZipEntry(path, "ppt/presentation.xml")
	if err != nil {
		return nil, err
	}
	var pres pptxPres
	if err := xml.Unmarshal(presXML, &pres); err != nil {
		return nil, fmt.Errorf("parse presentation.xml: %w", err)
	}
	m := &slideModel{}
	// 按 sldIdLst 顺序读取 slideN.xml（插件自产格式固定 slide1..slideN；
	// 不依赖 r:id 属性——Go encoding/xml 对带命名空间属性的匹配与
	// 无命名空间属性歧义（本地名相同），此处按序读取最稳健）。
	for i := range pres.SlideIDLst.IDs {
		page := slidePage{ID: fmt.Sprintf("s%d", i+1)}
		target := fmt.Sprintf("slides/slide%d.xml", i+1)
		slideXML, err := readZipEntry(path, "ppt/"+target)
		if err != nil {
			continue
		}
		var sld pptxSlide
		if err := xml.Unmarshal(slideXML, &sld); err != nil {
			continue
		}
		for _, sp := range sld.CSp.SpTree.Sps {
			for _, para := range sp.TxBody.Paras {
				var sb strings.Builder
				for _, r := range para.Runs {
					if r.Text != nil {
						sb.WriteString(r.Text.Text)
					}
				}
				if t := strings.TrimSpace(sb.String()); t != "" {
					if page.Title == "" {
						page.Title = t
					} else {
						page.Lines = append(page.Lines, t)
					}
				}
			}
		}
		// 图片回填：p:pic → r:embed → slide rels → zip 内 media 路径。
		for _, pic := range sld.CSp.SpTree.Pics {
			if pic.BlipFill.Blip.Embed == "" {
				continue
			}
			target, ok := slideRelTargets(path, i+1, pic.BlipFill.Blip.Embed)
			if !ok {
				continue
			}
			// 规范化：../media/image1.png → ppt/media/image1.png。
			rel := target
			if strings.HasPrefix(rel, "../") {
				rel = "ppt/" + strings.TrimPrefix(rel, "../")
			}
			zipName := strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/")
			data, err := readZipEntry(path, zipName)
			if err != nil {
				continue
			}
			page.ImagePath = zipName
			page.imageBytes = data
			break
		}
		if page.Title != "" || len(page.Lines) > 0 {
			m.Pages = append(m.Pages, page)
		}
	}
	return m, nil
}

// slideRelTargets 读取 slideN.xml.rels，返回 rId → Target 映射。
func slideRelTargets(path string, slideNo int, rID string) (string, bool) {
	b, err := readZipEntry(path, fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", slideNo))
	if err != nil {
		return "", false
	}
	rels := parseRels(b)
	target, ok := rels[rID]
	return target, ok
}

// slideImg 单页嵌入图片（已读入内存的二进制与元数据）。
type slideImg struct {
	zipName string // ppt/media/imageN.<ext>
	ext     string // png / jpeg / gif
	data    []byte
	width   int
	height  int
	picXML  string
}

// writeSlide 生成最小但完整合规的 pptx（每页标题 + 正文文本框 + 可选图片）。
//
// 合规要点（PowerPoint/WPS 打开无警告的前提）：
//   - [Content_Types].xml 为每个 part 声明 Default/Override；
//   - _rels/.rels 指向 presentation + docProps；
//   - presentation.xml 含 sldMasterIdLst + sldIdLst + sldSz；
//   - presentation.xml.rels 含 slideMaster/theme/presProps/viewProps/tableStyles 关系；
//   - slideMaster1.xml（+theme/layout 关系）、slideLayout1.xml（+master 关系）、
//     theme1.xml、presProps.xml、viewProps.xml、tableStyles.xml 全部在场；
//   - 每个 slideN.xml.rels 引用 slideLayout1.xml；
//   - 每页标题/正文为两个独立 <p:sp>（各含 1 个 nvSpPr + 1 个 txBody）。
func writeSlide(path string, m *slideModel) error {
	n := len(m.Pages)
	if n == 0 {
		n = 1 // 空文档也给出一页（office_new 的 pptx 容器语义）
	}
	// 图片根目录：GOFFICE_WORKSPACE（缺省用目标文件所在目录）。
	imgRoot := os.Getenv("GOFFICE_WORKSPACE")
	if imgRoot == "" {
		imgRoot = filepath.Dir(path)
	}
	slides := make([]string, 0, n)
	slideRels := make([]string, 0, n)
	var binFiles map[string][]byte
	imgExts := map[string]bool{}
	imgCount := 0
	for i := 0; i < n; i++ {
		p := slidePage{}
		if i < len(m.Pages) {
			p = m.Pages[i]
		}
		// 图片：读入 + 计算尺寸 + 生成 pic XML / rel XML。
		var img *slideImg
		if p.ImagePath != "" {
			var imgBytes []byte
			if len(p.imageBytes) > 0 {
				// 优先使用进程内缓存（readSlide 读回时填充），避免依赖工作区文件。
				imgBytes = p.imageBytes
			} else {
				var err error
				imgBytes, err = os.ReadFile(filepath.Join(imgRoot, filepath.FromSlash(p.ImagePath)))
				if err != nil {
					return fmt.Errorf("read image %q: %w", p.ImagePath, err)
				}
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(imgBytes))
			if err != nil {
				return fmt.Errorf("decode image %q: %w", p.ImagePath, err)
			}
			imgCount++
			ext := imageExt(filepath.Ext(p.ImagePath))
			if ext == "" {
				return fmt.Errorf("unsupported image format: %s", p.ImagePath)
			}
			imgExts[ext] = true
			zipName := fmt.Sprintf("ppt/media/image%d.%s", imgCount, ext)
			img = &slideImg{
				zipName: zipName,
				ext:     ext,
				data:    imgBytes,
				width:   cfg.Width,
				height:  cfg.Height,
			}
			if binFiles == nil {
				binFiles = map[string][]byte{}
			}
			binFiles[zipName] = imgBytes
			buildPicXML(img)
		}
		slides = append(slides, slideXML(p.Title, p.Lines, img))
		slideRels = append(slideRels, slideRelsXML(img))
	}
	files := map[string]string{
		"[Content_Types].xml":                          contentTypesXML(n, imgExts),
		"_rels/.rels":                                  rootRelsXML(),
		"docProps/core.xml":                            corePropsXML(),
		"docProps/app.xml":                             appPropsXML(n),
		"ppt/presentation.xml":                         presentationXML(n),
		"ppt/_rels/presentation.xml.rels":              presentationRelsXML(n),
		"ppt/presProps.xml":                            presPropsXML(),
		"ppt/viewProps.xml":                            viewPropsXML(),
		"ppt/tableStyles.xml":                          tableStylesXML(),
		"ppt/theme/theme1.xml":                         themeXML(),
		"ppt/slideMasters/slideMaster1.xml":            slideMasterXML(),
		"ppt/slideMasters/_rels/slideMaster1.xml.rels": slideMasterRelsXML(),
		"ppt/slideLayouts/slideLayout1.xml":            slideLayoutXML(),
		"ppt/slideLayouts/_rels/slideLayout1.xml.rels": slideLayoutRelsXML(),
	}
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("ppt/slides/slide%d.xml", i+1)] = slides[i]
		files[fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", i+1)] = slideRels[i]
	}
	return writeZipBytes(path, files, binFiles)
}

// imageExt 图片扩展名归一化（pptx 内统一 png/jpeg/gif）。
func imageExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "png"
	case ".jpg", ".jpeg":
		return "jpeg"
	case ".gif":
		return "gif"
	}
	return ""
}

// ---------- pptx 包内容（各 part 的合法最小 XML） ----------

// contentTypesXML 为每个 part 声明类型（Default ×2 + Override 全覆盖）。
// imgExts 为用到的图片扩展名集合（png/jpeg/gif），需额外声明 Default。
func contentTypesXML(n int, imgExts map[string]bool) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="` + nsContent + `"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/>`)
	for ext := range imgExts {
		ct := map[string]string{
			"png":  "image/png",
			"jpeg": "image/jpeg",
			"gif":  "image/gif",
		}[ext]
		fmt.Fprintf(&b, `<Default Extension="%s" ContentType="%s"/>`, ext, ct)
	}
	b.WriteString(`<Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>`)
	b.WriteString(`<Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml"/>`)
	b.WriteString(`<Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"/>`)
	b.WriteString(`<Override PartName="/ppt/theme/theme1.xml" ContentType="application/vnd.openxmlformats-officedocument.theme+xml"/>`)
	b.WriteString(`<Override PartName="/ppt/presProps.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presProps+xml"/>`)
	b.WriteString(`<Override PartName="/ppt/viewProps.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.viewProps+xml"/>`)
	b.WriteString(`<Override PartName="/ppt/tableStyles.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.tableStyles+xml"/>`)
	b.WriteString(`<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>`)
	b.WriteString(`<Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>`)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `<Override PartName="/ppt/slides/slide%d.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>`, i)
	}
	b.WriteString(`</Types>`)
	return b.String()
}

// rootRelsXML 包根关系：officeDocument + core/app 属性。
func rootRelsXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="` + nsPkgRel + `"><Relationship Id="rId1" Type="` + nsDocRel + `/officeDocument" Target="ppt/presentation.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/><Relationship Id="rId3" Type="` + nsDocRel + `/extended-properties" Target="docProps/app.xml"/></Relationships>`
}

// presentationXML 含 sldMasterIdLst（rId1→master）+ sldIdLst（rId2..）+ sldSz。
func presentationXML(n int) string {
	var ids strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&ids, `<p:sldId id="%d" r:id="rId%d"/>`, 256+i, 2+i)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:presentation xmlns:a="%s" xmlns:r="%s" xmlns:p="%s"><p:sldMasterIdLst><p:sldMasterId id="2147483648" r:id="rId1"/></p:sldMasterIdLst><p:sldIdLst>%s</p:sldIdLst><p:sldSz cx="12192000" cy="6858000"/><p:notesSz cx="6858000" cy="9144000"/></p:presentation>`,
		nsDrawing, nsDocRel, nsPML, ids.String())
}

// presentationRelsXML rId1=slideMaster，rId2..=slide，随后 theme/presProps/viewProps/tableStyles。
func presentationRelsXML(n int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="` + nsPkgRel + `">`)
	b.WriteString(`<Relationship Id="rId1" Type="` + nsDocRel + `/slideMaster" Target="slideMasters/slideMaster1.xml"/>`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="%s/slide" Target="slides/slide%d.xml"/>`, 2+i, nsDocRel, i+1)
	}
	base := 2 + n
	fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="%s/theme" Target="theme/theme1.xml"/>`, base, nsDocRel)
	fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="%s/presProps" Target="presProps.xml"/>`, base+1, nsDocRel)
	fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="%s/viewProps" Target="viewProps.xml"/>`, base+2, nsDocRel)
	fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="%s/tableStyles" Target="tableStyles.xml"/>`, base+3, nsDocRel)
	b.WriteString(`</Relationships>`)
	return b.String()
}

// slideXML 单页根：spTree 内 nvGrpSpPr+grpSpPr+两个独立 sp+可选 pic。
func slideXML(title string, lines []string, img *slideImg) string {
	body := slideBodyXML(title, lines)
	pic := ""
	if img != nil {
		pic = img.picXML
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:sld xmlns:a="%s" xmlns:r="%s" xmlns:p="%s"><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/>%s%s</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sld>`,
		nsDrawing, nsDocRel, nsPML, body, pic)
}

// slideBodyXML 构造文本框：标题/正文各为独立 <p:sp>（每个含 1 个 nvSpPr+spPr+txBody）。
func slideBodyXML(title string, lines []string) string {
	var sb strings.Builder
	// 标题框（独立 sp，id=2）。
	if title != "" {
		sb.WriteString(`<p:sp><p:nvSpPr><p:cNvPr id="2" name="Title"/><p:cNvSpPr txBox="1"/><p:nvPr/></p:nvSpPr><p:spPr><a:xfrm><a:off x="457200" y="457200"/><a:ext cx="11277600" cy="1447800"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr><p:txBody><a:bodyPr wrap="none"/><a:lstStyle/><a:p><a:r><a:rPr lang="zh-CN" sz="3000" b="1"/><a:t>` + xmlEscape(title) + `</a:t></a:r></a:p></p:txBody></p:sp>`)
	}
	// 正文框（独立 sp，id=3；每行一个 <a:p>）。
	if len(lines) > 0 {
		sb.WriteString(`<p:sp><p:nvSpPr><p:cNvPr id="3" name="Content"/><p:cNvSpPr txBox="1"/><p:nvPr/></p:nvSpPr><p:spPr><a:xfrm><a:off x="457200" y="2133600"/><a:ext cx="11277600" cy="4292600"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr><p:txBody><a:bodyPr wrap="none"/><a:lstStyle/>`)
		for _, ln := range lines {
			sb.WriteString(`<a:p><a:r><a:rPr lang="zh-CN" sz="1800"/><a:t>` + xmlEscape(ln) + `</a:t></a:r></a:p>`)
		}
		sb.WriteString(`</p:txBody></p:sp>`)
	}
	// 空页占位（单 sp，无文本）。
	if title == "" && len(lines) == 0 {
		sb.WriteString(`<p:sp><p:nvSpPr><p:cNvPr id="2" name="Empty"/><p:cNvSpPr txBox="1"/><p:nvPr/></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p/></p:txBody></p:sp>`)
	}
	return sb.String()
}

// slideRelsXML 每页引用 slideLayout1（rId1）；有图片时 rId2 指向图片。
func slideRelsXML(img *slideImg) string {
	rel := `<Relationship Id="rId1" Type="` + nsDocRel + `/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>`
	if img != nil {
		rel += `<Relationship Id="rId2" Type="` + nsDocRel + `/image" Target="../media/` + img.zipName[strings.LastIndex(img.zipName, "/")+1:] + `"/>`
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="` + nsPkgRel + `">` + rel + `</Relationships>`
}

// buildPicXML 构造 <p:pic>（cNvPr id=4，rId2 引用图片，等比缩放右侧区域）。
func buildPicXML(img *slideImg) {
	const (
		regionX = 7315200
		regionY = 2133600
		regionW = 4300000
		regionH = 4292600
	)
	w, h := img.width, img.height
	if w <= 0 || h <= 0 {
		w, h = regionW, regionH
	}
	scaleW := float64(regionW) / float64(w)
	scaleH := float64(regionH) / float64(h)
	scale := scaleW
	if scaleH < scale {
		scale = scaleH
	}
	drawW := int(float64(w) * scale)
	drawH := int(float64(h) * scale)
	offX := regionX + (regionW-drawW)/2
	offY := regionY + (regionH-drawH)/2
	img.picXML = fmt.Sprintf(`<p:pic><p:nvPicPr><p:cNvPr id="4" name="Picture 1"/><p:cNvPicPr><a:picLocks noChangeAspect="1"/></p:cNvPicPr><p:nvPr/></p:nvPicPr><p:blipFill><a:blip r:embed="rId2"/><a:stretch><a:fillRect/></a:stretch></p:blipFill><p:spPr><a:xfrm><a:off x="%d" y="%d"/><a:ext cx="%d" cy="%d"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr></p:pic>`, offX, offY, drawW, drawH)
}

// slideMasterXML 母版：bg + 空 spTree + clrMap + sldLayoutIdLst(rId1→layout) + txStyles。
func slideMasterXML() string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:sldMaster xmlns:a="%s" xmlns:r="%s" xmlns:p="%s"><p:cSld><p:bg><p:bgPr><a:solidFill><a:srgbClr val="FFFFFF"/></a:solidFill><a:effectLst/></p:bgPr></p:bg><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/></p:spTree></p:cSld><p:clrMap bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/><p:sldLayoutIdLst><p:sldLayoutId id="2147483649" r:id="rId1"/></p:sldLayoutIdLst><p:txStyles><p:titleStyle><a:lvl1pPr algn="l" rtl="0" eaLnBrk="1" latinLnBrk="0" hangingPunct="1"><a:defRPr sz="3000" kern="1200"/></a:lvl1pPr></p:titleStyle><p:bodyStyle><a:lvl1pPr marL="0" algn="l" rtl="0" eaLnBrk="1" latinLnBrk="0" hangingPunct="1"><a:defRPr sz="1800" kern="1200"/></a:lvl1pPr></p:bodyStyle><p:otherStyle><a:defPPr/><a:lvl1pPr algn="l" rtl="0" eaLnBrk="1" latinLnBrk="0" hangingPunct="1"><a:defRPr lang="en-US" sz="1800" kern="1200"/></a:lvl1pPr></p:otherStyle></p:txStyles></p:sldMaster>`,
		nsDrawing, nsDocRel, nsPML)
}

// slideMasterRelsXML 母版关系：rId1=layout，rId2=theme。
func slideMasterRelsXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="` + nsPkgRel + `"><Relationship Id="rId1" Type="` + nsDocRel + `/slideLayout" Target="../slideLayouts/slideLayout1.xml"/><Relationship Id="rId2" Type="` + nsDocRel + `/theme" Target="../theme/theme1.xml"/></Relationships>`
}

// slideLayoutXML 空白版式：空 spTree + masterClrMapping。
func slideLayoutXML() string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:sldLayout xmlns:a="%s" xmlns:r="%s" xmlns:p="%s" type="blank"><p:cSld name="Blank"><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/></p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sldLayout>`,
		nsDrawing, nsDocRel, nsPML)
}

// slideLayoutRelsXML 版式引用母版。
func slideLayoutRelsXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="` + nsPkgRel + `"><Relationship Id="rId1" Type="` + nsDocRel + `/slideMaster" Target="../slideMasters/slideMaster1.xml"/></Relationships>`
}

// themeXML 精简但齐全的主题（clrScheme/fontScheme/fmtScheme）：默认 Office 配色。
func themeXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><a:theme xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" name="Office Theme"><a:themeElements><a:clrScheme name="Office"><a:dk1><a:sysClr val="windowText" lastClr="000000"/></a:dk1><a:lt1><a:sysClr val="window" lastClr="FFFFFF"/></a:lt1><a:dk2><a:srgbClr val="44546A"/></a:dk2><a:lt2><a:srgbClr val="E7E6E6"/></a:lt2><a:accent1><a:srgbClr val="4472C4"/></a:accent1><a:accent2><a:srgbClr val="ED7D31"/></a:accent2><a:accent3><a:srgbClr val="A5A5A5"/></a:accent3><a:accent4><a:srgbClr val="FFC000"/></a:accent4><a:accent5><a:srgbClr val="5B9BD5"/></a:accent5><a:accent6><a:srgbClr val="70AD47"/></a:accent6><a:hlink><a:srgbClr val="0563C1"/></a:hlink><a:folHlink><a:srgbClr val="954F72"/></a:folHlink></a:clrScheme><a:fontScheme name="Office"><a:majorFont><a:latin typeface="Calibri Light" panose="020F0302020204030204"/><a:ea typeface=""/><a:cs typeface=""/></a:majorFont><a:minorFont><a:latin typeface="Calibri" panose="020F0502020204030204"/><a:ea typeface=""/><a:cs typeface=""/></a:minorFont></a:fontScheme><a:fmtScheme name="Office"><a:fillStyleLst><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:fillStyleLst><a:lnStyleLst><a:ln w="6350" cap="flat" cmpd="sng" algn="ctr"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:prstDash val="solid"/><a:miter lim="800000"/></a:ln><a:ln w="12700" cap="flat" cmpd="sng" algn="ctr"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:prstDash val="solid"/><a:miter lim="800000"/></a:ln><a:ln w="19050" cap="flat" cmpd="sng" algn="ctr"><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:prstDash val="solid"/><a:miter lim="800000"/></a:ln></a:lnStyleLst><a:effectStyleLst><a:effectStyle><a:effectLst/></a:effectStyle><a:effectStyle><a:effectLst/></a:effectStyle><a:effectStyle><a:effectLst/></a:effectStyle></a:effectStyleLst><a:bgFillStyleLst><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:solidFill><a:schemeClr val="phClr"/></a:solidFill><a:solidFill><a:schemeClr val="phClr"/></a:solidFill></a:bgFillStyleLst></a:fmtScheme></a:themeElements></a:theme>`
}

// presPropsXML 展示属性（空容器即可，所有子元素可选）。
func presPropsXML() string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:presentationPr xmlns:a="%s" xmlns:r="%s" xmlns:p="%s"/>`, nsDrawing, nsDocRel, nsPML)
}

// viewPropsXML 视图属性（normal/slide/notes 最小视图状态）。
func viewPropsXML() string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:viewPr xmlns:a="%s" xmlns:r="%s" xmlns:p="%s"><p:normalViewPr><p:restoredLeftSz sz="15620"/><p:restoredTopSz sz="94660"/></p:normalViewPr><p:slideViewPr><p:cSldViewPr><p:cViewPr varScale="1"><p:scale><a:sx n="100" d="100"/><a:sy n="100" d="100"/></p:scale><p:origin x="0" y="0"/></p:cViewPr><p:guideLst/></p:cSldViewPr></p:slideViewPr><p:notesTextViewPr><p:cViewPr><p:scale><a:sx n="100" d="100"/><a:sy n="100" d="100"/></p:scale><p:origin x="0" y="0"/></p:cViewPr></p:notesTextViewPr><p:gridSpacing cx="72008" cy="72008"/></p:viewPr>`, nsDrawing, nsDocRel, nsPML)
}

// tableStylesXML 表格样式表（空容器 + 默认样式 GUID）。
func tableStylesXML() string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:tblStyleLst xmlns:a="%s" xmlns:r="%s" xmlns:p="%s" def="{5C22544A-7EE6-4342-B048-85BDC9FD1C3A}"/>`, nsDrawing, nsDocRel, nsPML)
}

// corePropsXML 核心属性。
func corePropsXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><dc:title>Kaelvoris Presentation</dc:title><dc:creator>Kaelvoris</dc:creator><cp:lastModifiedBy>Kaelvoris</cp:lastModifiedBy><dcterms:created xsi:type="dcterms:W3CDTF">2026-01-01T00:00:00Z</dcterms:created><dcterms:modified xsi:type="dcterms:W3CDTF">2026-01-01T00:00:00Z</dcterms:modified></cp:coreProperties>`
}

// appPropsXML 扩展属性（Slides 计数）。
func appPropsXML(n int) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties" xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes"><Application>Kaelvoris</Application><Slides>%d</Slides><Notes>0</Notes><HiddenSlides>0</HiddenSlides><MMClips>0</MMClips><ScaleCrop>false</ScaleCrop><LinksUpToDate>false</LinksUpToDate><SharedDoc>false</SharedDoc><HyperlinksChanged>false</HyperlinksChanged><AppVersion>16.0000</AppVersion></Properties>`, n)
}

// loadSlide 读取 pptx（容错：返回空模型）。
func loadSlide(path string) (*slideModel, error) {
	m, err := readSlide(path)
	if err != nil {
		return &slideModel{}, nil
	}
	return m, nil
}

// ---------- zip 工具 ----------

// writeZip 按 map（文本内容）写 zip 包。
func writeZip(path string, files map[string]string) error {
	return writeZipBytes(path, files, nil)
}

// writeZipBytes 写 zip 包：文本条目 + 二进制条目（图片等）。
func writeZipBytes(path string, files map[string]string, binFiles map[string][]byte) error {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, content); err != nil {
			return err
		}
	}
	for name, data := range binFiles {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// readZipEntry 读取 zip 包内单个条目。
func readZipEntry(path, name string) ([]byte, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("zip entry %q not found", name)
}

// parseRels 解析 .rels XML 为 (Id → Target) 映射。
func parseRels(b []byte) map[string]string {
	out := map[string]string{}
	var relXML struct {
		XMLName xml.Name `xml:"Relationships"`
		Rels    []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(b, &relXML); err != nil {
		return out
	}
	for _, r := range relXML.Rels {
		out[r.ID] = r.Target
	}
	return out
}

func xmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

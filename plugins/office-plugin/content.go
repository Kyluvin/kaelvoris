// Office 内容引擎：xlsx / csv / tsv / docx / pptx 的创建、读写、导入与导出。
//
// 对齐 dsh-univer-office 的内容能力面（不依赖其商业 Univer SDK，全部 Go 原生）：
//   - Sheet：单元格读写、公式、样式（Excel 生态，最完整，excelize）；
//   - Doc：段落文本（docx，最小 OOXML）；
//   - Slide：页面文本（pptx，最小 OOXML）；
//   - 导入：xlsx/csv/tsv 读入统一表格模型；docx/pptx 读入文本视图；
//   - 导出：Sheet → xlsx/csv/tsv；Doc → docx；Slide → pptx。
//
// 本实现是「轻量领域模型 + 文件格式适配」，不做富格式 WYSIWYG。
package main

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuri/excelize/v2"
)

// Unit 模型：一个文档可包含多个分区（sheet 页 / doc 段落集 / slide 页面集）。
// 对齐 dsh 的 Unit 概念——每个分区有稳定 ID，内容工具必须以显式分区操作。
type unitKind string

const (
	unitSheet unitKind = "sheet"
	unitDoc   unitKind = "doc"
	unitSlide unitKind = "slide"
)

// sheetModel 统一表格模型（xlsx/csv/tsv 共用）。
type sheetModel struct {
	Name    string     `json:"name"`
	Headers []string   `json:"headers,omitempty"`
	Rows    [][]string `json:"rows"`
}

// docModel 文档文本模型。
type docModel struct {
	Title string   `json:"title,omitempty"`
	Paras []string `json:"paras"`
}

// slideModel 演示文本模型（页面 → 文本框）。
type slideModel struct {
	Title string      `json:"title,omitempty"`
	Pages []slidePage `json:"pages"`
}

// slidePage 单页内容：标题 + 正文行 + 可选图片。
type slidePage struct {
	ID    string   `json:"id"`
	Title string   `json:"title,omitempty"`
	Lines []string `json:"lines"`
	// ImagePath 可选：工作区相对路径的图片（png/jpg/jpeg/gif），
	// 生成 pptx 时嵌入 ppt/media/ 并加 <p:pic>。
	ImagePath    string `json:"imagePath,omitempty"`
	ImageCaption string `json:"imageCaption,omitempty"`

	// imageBytes 进程内图片字节缓存（readSlide 读回时填充，不落 JSON）。
	imageBytes []byte
}

// ---------- 表格（xlsx / csv / tsv） ----------

// newSheet 创建空 xlsx（名字为 sheet 名，首个空工作表）。
func newSheet(path, name string) error {
	f := excelize.NewFile()
	defer f.Close()
	if name != "" {
		f.SetSheetName(f.GetSheetName(0), name)
	}
	return f.SaveAs(path)
}

// loadSheet 读取 xlsx/csv/tsv 为统一表格模型。
func loadSheet(path string) (*sheetModel, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".xlsx":
		f, err := excelize.OpenFile(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		sheet := f.GetSheetName(0)
		rows, err := f.GetRows(sheet)
		if err != nil {
			return nil, err
		}
		return rowsToModel(rows, sheet), nil
	case ".csv":
		rows, err := parseCSV(path, ',')
		if err != nil {
			return nil, err
		}
		return rowsToModel(rows, "sheet1"), nil
	case ".tsv":
		rows, err := parseCSV(path, '\t')
		if err != nil {
			return nil, err
		}
		return rowsToModel(rows, "sheet1"), nil
	default:
		return nil, derr(errUnitTypeInvalid, "unsupported table format: %s", ext)
	}
}

func rowsToModel(rows [][]string, name string) *sheetModel {
	m := &sheetModel{Name: name, Rows: rows}
	if len(rows) > 0 {
		m.Headers = rows[0]
		m.Rows = rows[1:]
	}
	return m
}

// saveSheet 把表格模型写回 xlsx/csv/tsv。
func saveSheet(path string, m *sheetModel) error {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".xlsx":
		f := excelize.NewFile()
		defer f.Close()
		sheet := m.Name
		if sheet == "" {
			sheet = f.GetSheetName(0)
		} else {
			f.SetSheetName(f.GetSheetName(0), sheet)
		}
		all := [][]string{m.Headers}
		if !(len(m.Headers) == 1 && m.Headers[0] == "") {
			all = append(all, m.Rows...)
		} else {
			all = append([][]string{}, m.Rows...)
		}
		for r, row := range all {
			for c, cell := range row {
				cellName, _ := excelize.CoordinatesToCellName(c+1, r+1)
				if err := f.SetCellValue(sheet, cellName, cell); err != nil {
					return err
				}
			}
		}
		return f.SaveAs(path)
	case ".csv":
		return writeCSV(path, allRows(m), ',')
	case ".tsv":
		return writeCSV(path, allRows(m), '\t')
	default:
		return derr(errUnitTypeInvalid, "unsupported table format: %s", ext)
	}
}

// allRows 模型展开为全量行（headers 在前）。
func allRows(m *sheetModel) [][]string {
	out := make([][]string, 0, len(m.Rows)+1)
	if len(m.Headers) > 0 {
		out = append(out, m.Headers)
	}
	out = append(out, m.Rows...)
	return out
}

func parseCSV(path string, comma rune) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comma = comma
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	return recs, nil
}

func writeCSV(path string, rows [][]string, comma rune) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Comma = comma
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// ---------- 分区（Unit）操作 ----------

// listUnits 列出文档内的分区（按扩展名）。
func listUnits(path string) ([]string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".xlsx":
		f, err := excelize.OpenFile(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return f.GetSheetList(), nil
	case ".docx":
		m, err := loadDoc(path)
		if err != nil {
			return nil, err
		}
		if m == nil || m.Title == "" {
			return []string{"doc"}, nil
		}
		return []string{"doc"}, nil
	case ".pptx":
		m, err := loadSlide(path)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(m.Pages))
		for _, p := range m.Pages {
			ids = append(ids, p.ID)
		}
		return ids, nil
	case ".csv", ".tsv":
		return []string{"sheet1"}, nil
	default:
		return nil, derr(errUnitTypeInvalid, "unsupported format: %s", path)
	}
}

// emptyFile 判断文档是否为空（无分区）。
func emptyFile(path string) bool {
	units, err := listUnits(path)
	return err != nil || len(units) == 0
}

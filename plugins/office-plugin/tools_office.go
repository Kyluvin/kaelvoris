// 领域工具定义（对齐 dsh-univer-office 的 univer_* 工具集，映射为 office_*）。
//
// 工具清单与 dsh 的对应关系：
//
//	office_new        ← univer_new        创建空文档（不覆盖）
//	office_status     ← univer_status     查看文档与草稿状态（发现 Unit）
//	office_worktree   ← univer_worktree   草稿生命周期（create/ready/reopen/merge/discard）
//	office_unit       ← univer_unit       添加/删除内容分区
//	office_inspect    ← univer_inspect    读取文档结构/表格范围
//	office_execute    ← univer_execute    直接编辑内容（单元格/段落/页面）
//	office_export     ← univer_export     导出 xlsx/csv/tsv/docx/pptx
//	office_verify     ← univer_lint+截图 结构读回与校验（轻量版）
//
// 内容写操作统一进入草稿（worktree），merge/discard 需用户显式审批——
// 复用 Kaelvoris 的 ask 桥（引擎对工具 ask 审批，见 internal/agent/engine.go）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// ctxState 工具执行上下文（进程内单例）。
type ctxState struct {
	store *wtStore
}

// pluginWS 当前插件工作区（GOFFICE_WORKSPACE，缺省当前目录）。
func pluginWS() string {
	ws := os.Getenv("GOFFICE_WORKSPACE")
	if ws == "" {
		ws = "."
	}
	return ws
}

// newCtxState 构建上下文（工作区取 GOFFICE_WORKSPACE 环境变量，缺省当前目录）。
func newCtxState() *ctxState {
	ws := os.Getenv("GOFFICE_WORKSPACE")
	if ws == "" {
		ws = "."
	}
	return &ctxState{store: newWTStore(ws)}
}

// registerNewTool 注册 office_new。
func registerNewTool(s *Server) {
	s.register(&Tool{
		Name: "office_new",
		Description: "Create a new empty office document (.xlsx/.csv/.tsv/.docx/.pptx) in the workspace. " +
			"Never overwrites an existing file and does not create an implicit unit; use office_unit or " +
			"office_import to add content. Supported: xlsx (spreadsheet), csv/tsv (table data), docx (document), pptx (presentation).",
		Parameters: objectSchema(map[string]interface{}{
			"file": strPropReq("Workspace-relative path ending in .xlsx/.csv/.tsv/.docx/.pptx. Creates an empty container; existing file is an error."),
		}, []string{"file"}),
		Execute: func(args json.RawMessage) (string, error) {
			var p struct {
				File string `json:"file"`
			}
			if err := bindArgs(args, &p); err != nil {
				return "", err
			}
			rel, err := resolveScope(pluginWS(), p.File)
			if err != nil {
				return "", err
			}
			abs := trunkPath(pluginWS(), rel)
			if _, err := os.Stat(abs); err == nil {
				return "", derr(errFileExists, "file %q already exists; choose another name (office_new never overwrites)", rel)
			}
			switch strings.ToLower(filepath.Ext(rel)) {
			case ".xlsx":
				if err := newSheet(abs, "sheet1"); err != nil {
					return "", err
				}
			case ".csv":
				if err := writeCSV(abs, [][]string{}, ','); err != nil {
					return "", err
				}
			case ".tsv":
				if err := writeCSV(abs, [][]string{}, '\t'); err != nil {
					return "", err
				}
			case ".docx":
				if err := writeDocx(abs, &docModel{}); err != nil {
					return "", err
				}
			case ".pptx":
				if err := writeSlide(abs, &slideModel{Pages: []slidePage{{ID: "1"}}}); err != nil {
					return "", err
				}
			default:
				return "", derr(errUnitTypeInvalid, "unsupported extension %q (want .xlsx/.csv/.tsv/.docx/.pptx)", filepath.Ext(rel))
			}
			return fmt.Sprintf("Created empty office file: %s\nExt: %s\nUse office_worktree action=create, then office_unit/office_execute to author content.", rel, filepath.Ext(rel)), nil
		},
	})
}

// registerStatusTool 注册 office_status。
func registerStatusTool(s *Server) {
	s.register(&Tool{
		Name: "office_status",
		Description: "View an office file's state: whether it exists, its units, and all worktrees (drafts) with status. " +
			"Call this before picking a unitId or continuing a prior draft. Optionally filter to one worktree.",
		Parameters: objectSchema(map[string]interface{}{
			"file":       strPropReq("Workspace-relative or absolute .xlsx/.docx/.pptx/.csv/.tsv path."),
			"worktreeId": strProp("Optional worktree id to describe only."),
		}, []string{"file"}),
		Execute: func(args json.RawMessage) (string, error) {
			var p struct {
				File       string `json:"file"`
				WorktreeID string `json:"worktreeId"`
			}
			if err := bindArgs(args, &p); err != nil {
				return "", err
			}
			rel, err := resolveScope(pluginWS(), p.File)
			if err != nil {
				return "", err
			}
			st := newCtxState()
			abs := trunkPath(pluginWS(), rel)
			exists := false
			if _, err := os.Stat(abs); err == nil {
				exists = true
			}
			units, _ := listUnits(abs)
			wts := st.store.listWorktrees(rel)
			var b strings.Builder
			if !exists {
				b.WriteString(fmt.Sprintf("File %q does not exist yet (create with office_new).\n", rel))
			} else {
				b.WriteString(fmt.Sprintf("File: %s\nTrunk: present\n", rel))
				if len(units) == 0 {
					b.WriteString("Units: none (empty container)\n")
				} else {
					b.WriteString("Units: " + strings.Join(units, ", ") + "\n")
				}
			}
			if p.WorktreeID != "" {
				for _, wt := range wts {
					if wt.ID == p.WorktreeID {
						b.WriteString(fmt.Sprintf("\nWorktree %s: %s (created %s)\n", wt.ID, wt.Status, wt.CreatedAt.Format(time.RFC3339)))
						return b.String(), nil
					}
				}
				return "", derr(errWorktreeState, "worktree %q not found for %q", p.WorktreeID, rel)
			}
			b.WriteString("\nWorktrees:\n")
			if len(wts) == 0 {
				b.WriteString("  (none)\n")
			}
			for _, wt := range wts {
				b.WriteString(fmt.Sprintf("  - %s [%s] %s (updated %s)\n", wt.ID, wt.Status, wt.Name, wt.UpdatedAt.Format(time.RFC3339)))
			}
			if exists {
				b.WriteString("\nTip: create a draft with office_worktree action=create before editing. merge/discard need explicit user approval.")
			}
			return b.String(), nil
		},
	})
}

// registerWorktreeTool 注册 office_worktree（生命周期 + 审批语义）。
func registerWorktreeTool(s *Server) {
	s.register(&Tool{
		Name: "office_worktree",
		Description: "Create or transition an isolated draft (worktree) for an office file. Actions: create (copy trunk to draft), " +
			"ready (mark changes done, waiting for review), reopen (ready→draft, continue editing), merge (publish draft over trunk), " +
			"discard (permanently remove draft changes). CRITICAL: merge and discard change review state and require explicit user request/approval — never call them autonomously.",
		Parameters: objectSchema(map[string]interface{}{
			"action":     strEnum("Lifecycle action.", "create", "ready", "reopen", "merge", "discard"),
			"file":       strPropReq("Workspace-relative or absolute office file path."),
			"worktreeId": strProp("Required for every action except create."),
			"name":       strProp("Optional human-readable draft name for create."),
		}, []string{"action", "file"}),
		Execute: func(args json.RawMessage) (string, error) {
			var p struct {
				Action     string `json:"action"`
				File       string `json:"file"`
				WorktreeID string `json:"worktreeId"`
				Name       string `json:"name"`
			}
			if err := bindArgs(args, &p); err != nil {
				return "", err
			}
			if p.Action == "" {
				return "", derr(errInvalidRequest, "action is required (create/ready/reopen/merge/discard)")
			}
			rel, err := resolveScope(pluginWS(), p.File)
			if err != nil {
				return "", err
			}
			st := newCtxState()
			switch p.Action {
			case "create":
				meta, err := st.store.createWorktree(pluginWS(), rel, p.Name, "")
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Created worktree %s [%s] for %s\nDraft copied from trunk. Edit via office_execute/office_unit. Mark ready via office_worktree action=ready when done.", meta.ID, meta.Status, rel), nil
			case "ready":
				if p.WorktreeID == "" {
					return "", derr(errInvalidRequest, "worktreeId required for ready")
				}
				if err := st.store.readyWorktree(rel, p.WorktreeID); err != nil {
					return "", err
				}
				return fmt.Sprintf("Worktree %s marked ready for %s. Changes await user review. Continue? office_worktree action=reopen.", p.WorktreeID, rel), nil
			case "reopen":
				if p.WorktreeID == "" {
					return "", derr(errInvalidRequest, "worktreeId required for reopen")
				}
				if err := st.store.reopenWorktree(rel, p.WorktreeID); err != nil {
					return "", err
				}
				return fmt.Sprintf("Worktree %s reopened (draft). Continue editing via office_execute.", p.WorktreeID), nil
			case "merge":
				if p.WorktreeID == "" {
					return "", derr(errInvalidRequest, "worktreeId required for merge")
				}
				if err := st.store.mergeWorktree(pluginWS(), rel, p.WorktreeID); err != nil {
					return "", err
				}
				return fmt.Sprintf("Worktree %s merged into trunk for %s. Draft is now terminal (merged); create a new one for further edits.", p.WorktreeID, rel), nil
			case "discard":
				if p.WorktreeID == "" {
					return "", derr(errInvalidRequest, "worktreeId required for discard")
				}
				if err := st.store.discardWorktree(rel, p.WorktreeID); err != nil {
					return "", err
				}
				return fmt.Sprintf("Worktree %s discarded for %s. Trunk unchanged.", p.WorktreeID, rel), nil
			default:
				return "", derr(errInvalidRequest, "unknown action %q", p.Action)
			}
		},
	})
}

// registerUnitTool 注册 office_unit。
func registerUnitTool(s *Server) {
	s.register(&Tool{
		Name: "office_unit",
		Description: "Add or list content units inside a draft worktree. Units are content partitions: sheet pages (xlsx), " +
			"the document body (docx), or slide pages (pptx). The model must know the worktreeId and unitId before editing.",
		Parameters: objectSchema(map[string]interface{}{
			"action":     strEnum("Unit action.", "list", "add", "remove"),
			"file":       strPropReq("Workspace-relative office file path."),
			"worktreeId": strProp("Draft worktree id (required for add/remove)."),
			"unitId":     strProp("Unit id to remove."),
			"name":       strProp("Optional unit display name (add)."),
		}, []string{"action", "file"}),
		Execute: func(args json.RawMessage) (string, error) {
			var p struct {
				Action     string `json:"action"`
				File       string `json:"file"`
				WorktreeID string `json:"worktreeId"`
				UnitID     string `json:"unitId"`
				Name       string `json:"name"`
			}
			if err := bindArgs(args, &p); err != nil {
				return "", err
			}
			rel, err := resolveScope(pluginWS(), p.File)
			if err != nil {
				return "", err
			}
			st := newCtxState()
			switch p.Action {
			case "list":
				units, err := listUnits(trunkPath(pluginWS(), rel))
				if err != nil {
					return "", err
				}
				if len(units) == 0 {
					return "No units in file (empty container). Use office_import or office_execute to add content.", nil
				}
				return "Units: " + strings.Join(units, ", "), nil
			case "add", "remove":
				if p.WorktreeID == "" {
					return "", derr(errInvalidRequest, "worktreeId required for %s", p.Action)
				}
				// add/remove 目前仅记录 unit 名（内容编辑由 office_execute 直接建模）。
				meta := st.store.readMeta(rel, p.WorktreeID)
				if meta == nil {
					return "", derr(errWorktreeState, "worktree %q not found", p.WorktreeID)
				}
				if p.Action == "add" {
					meta.Units = appendUnique(meta.Units, p.Name)
					if err := st.store.writeMeta(rel, meta); err != nil {
						return "", err
					}
					return fmt.Sprintf("Unit %q added to worktree %s (metadata only; author content via office_execute).", p.Name, p.WorktreeID), nil
				}
				meta.Units = removeString(meta.Units, p.UnitID)
				if err := st.store.writeMeta(rel, meta); err != nil {
					return "", err
				}
				return fmt.Sprintf("Unit %q removed from worktree %s.", p.UnitID, p.WorktreeID), nil
			default:
				return "", derr(errInvalidRequest, "unknown action %q", p.Action)
			}
		},
	})
}

// registerInspectTool 注册 office_inspect。
func registerInspectTool(s *Server) {
	s.register(&Tool{
		Name: "office_inspect",
		Description: "Read structured content of an office file (trunk or draft). For xlsx/csv/tsv: table rows with headers. " +
			"For docx: paragraphs. For pptx: pages with title and lines. Returns text summaries, prefer over shell cat.",
		Parameters: objectSchema(map[string]interface{}{
			"file":       strPropReq("Workspace-relative office file path."),
			"worktreeId": strProp("Read from this draft (omit for trunk)."),
			"unitId":     strProp("Optional unit id (sheet name / page id). Omit = whole file."),
			"maxRows":    strProp("Optional row/page limit (default 50)."),
		}, []string{"file"}),
		Execute: func(args json.RawMessage) (string, error) {
			var p struct {
				File       string `json:"file"`
				WorktreeID string `json:"worktreeId"`
				UnitID     string `json:"unitId"`
				MaxRows    int    `json:"maxRows"`
			}
			if err := bindArgs(args, &p); err != nil {
				return "", err
			}
			rel, err := resolveScope(pluginWS(), p.File)
			if err != nil {
				return "", err
			}
			if p.MaxRows <= 0 {
				p.MaxRows = 50
			}
			path := trunkPath(pluginWS(), rel)
			if p.WorktreeID != "" {
				path = newCtxState().store.draftPath(rel, p.WorktreeID)
			}
			if _, err := os.Stat(path); err != nil {
				return "", derr(errFileNotFound, "file %q not found (or draft missing)", rel)
			}
			ext := strings.ToLower(filepath.Ext(path))
			switch ext {
			case ".xlsx", ".csv", ".tsv":
				m, err := loadSheet(path)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				b.WriteString(fmt.Sprintf("Sheet %q (%d rows x %d cols)\n", m.Name, len(allRows(m)), maxCols(allRows(m))))
				if len(m.Headers) > 0 {
					b.WriteString("Headers: " + strings.Join(m.Headers, " | ") + "\n")
				}
				for i, row := range m.Rows {
					if i >= p.MaxRows {
						b.WriteString(fmt.Sprintf("... (%d more rows)\n", len(m.Rows)-p.MaxRows))
						break
					}
					b.WriteString(fmt.Sprintf("%4d | %s\n", i+2, strings.Join(row, " | ")))
				}
				return b.String(), nil
			case ".docx":
				m, err := loadDoc(path)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				b.WriteString("Document paragraphs:\n")
				for i, para := range m.Paras {
					if i >= p.MaxRows {
						b.WriteString(fmt.Sprintf("... (%d more)\n", len(m.Paras)-p.MaxRows))
						break
					}
					b.WriteString(fmt.Sprintf("%4d | %s\n", i+1, para))
				}
				return b.String(), nil
			case ".pptx":
				m, err := loadSlide(path)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				b.WriteString(fmt.Sprintf("Presentation (%d pages)\n", len(m.Pages)))
				for i, pg := range m.Pages {
					if i >= p.MaxRows {
						b.WriteString(fmt.Sprintf("... (%d more pages)\n", len(m.Pages)-p.MaxRows))
						break
					}
					b.WriteString(fmt.Sprintf("Page %s: %s\n", pg.ID, pg.Title))
					for _, ln := range pg.Lines {
						b.WriteString("    - " + ln + "\n")
					}
				}
				return b.String(), nil
			default:
				return "", derr(errUnitTypeInvalid, "unsupported format: %s", ext)
			}
		},
	})
}

// registerExecuteTool 注册 office_execute（内容编辑核心）。
func registerExecuteTool(s *Server) {
	s.register(&Tool{
		Name: "office_execute",
		Description: "Edit office content in a draft worktree. Actions: " +
			"set_cell (xlsx: put value at sheet/col/row), set_range (xlsx: fill a block of rows), " +
			"set_paras (docx: replace paragraphs), add_page/set_page (pptx: add or replace a page's title/lines). " +
			"Read back with office_inspect after writing. Persists only when mutations occurred.",
		Parameters: objectSchema(map[string]interface{}{
			"file":         strPropReq("Workspace-relative office file path."),
			"worktreeId":   strPropReq("Draft worktree id (must be draft or reopened ready)."),
			"unitId":       strProp("Unit id: sheet name, 'doc', or page id."),
			"action":       strEnum("Mutation action.", "set_cell", "set_range", "set_paras", "add_page", "set_page"),
			"value":        strProp("Value for set_cell."),
			"row":          strProp("1-based row for set_cell/set_range."),
			"col":          strProp("Column letter or 1-based number for set_cell."),
			"rows":         strProp("JSON array of arrays for set_range (headers + data)."),
			"paras":        strProp("JSON array of strings for set_paras."),
			"pageTitle":    strProp("Page title for add_page/set_page."),
			"lines":        strProp("JSON array of strings for add_page/set_page."),
			"imagePath":    strProp("Workspace-relative image file (png/jpg/gif) for add_page/set_page (pptx)."),
			"imageCaption": strProp("Optional caption below the image (pptx)."),
		}, []string{"file", "worktreeId", "action"}),
		Execute: func(args json.RawMessage) (string, error) {
			var p struct {
				File         string `json:"file"`
				WorktreeID   string `json:"worktreeId"`
				UnitID       string `json:"unitId"`
				Action       string `json:"action"`
				Value        string `json:"value"`
				Row          string `json:"row"`
				Col          string `json:"col"`
				Rows         string `json:"rows"`
				Paras        string `json:"paras"`
				PageTitle    string `json:"pageTitle"`
				Lines        string `json:"lines"`
				ImagePath    string `json:"imagePath"`
				ImageCaption string `json:"imageCaption"`
			}
			if err := bindArgs(args, &p); err != nil {
				return "", err
			}
			if p.Action == "" {
				return "", derr(errInvalidRequest, "action required (set_cell/set_range/set_paras/add_page/set_page)")
			}
			rel, err := resolveScope(pluginWS(), p.File)
			if err != nil {
				return "", err
			}
			st := newCtxState()
			meta := st.store.readMeta(rel, p.WorktreeID)
			if meta == nil {
				return "", derr(errWorktreeState, "worktree %q not found; create it with office_worktree action=create", p.WorktreeID)
			}
			if meta.Status != statusDraft {
				return "", derr(errWorktreeState, "worktree %q is %s; only draft accepts writes (reopen first)", p.WorktreeID, meta.Status)
			}
			draft := st.store.draftPath(rel, p.WorktreeID)
			ext := strings.ToLower(filepath.Ext(draft))
			switch p.Action {
			case "set_cell":
				if ext != ".xlsx" {
					return "", derr(errUnitTypeInvalid, "set_cell only for xlsx (file is %s)", ext)
				}
				if err := setCell(draft, p.UnitID, p.Row, p.Col, p.Value); err != nil {
					return "", err
				}
				return fmt.Sprintf("Cell %s%s = %q written to draft. Verify: office_inspect file=%s worktreeId=%s", p.Col, p.Row, p.Value, rel, p.WorktreeID), nil
			case "set_range":
				if ext != ".xlsx" {
					return "", derr(errUnitTypeInvalid, "set_range only for xlsx (file is %s)", ext)
				}
				var rows [][]string
				if err := json.Unmarshal([]byte(p.Rows), &rows); err != nil {
					return "", derr(errInvalidRequest, "rows must be JSON array of arrays: %v", err)
				}
				if err := setRange(draft, p.UnitID, rows); err != nil {
					return "", err
				}
				return fmt.Sprintf("Wrote %d rows to %s draft.", len(rows), rel), nil
			case "set_paras":
				if ext != ".docx" {
					return "", derr(errUnitTypeInvalid, "set_paras only for docx (file is %s)", ext)
				}
				var paras []string
				if err := json.Unmarshal([]byte(p.Paras), &paras); err != nil {
					return "", derr(errInvalidRequest, "paras must be JSON array of strings: %v", err)
				}
				if err := writeDocx(draft, &docModel{Paras: paras}); err != nil {
					return "", err
				}
				return fmt.Sprintf("Wrote %d paragraphs to docx draft.", len(paras)), nil
			case "add_page", "set_page":
				if ext != ".pptx" {
					return "", derr(errUnitTypeInvalid, "add_page/set_page only for pptx (file is %s)", ext)
				}
				m, err := loadSlide(draft)
				if err != nil {
					return "", err
				}
				var lines []string
				if p.Lines != "" {
					if err := json.Unmarshal([]byte(p.Lines), &lines); err != nil {
						return "", derr(errInvalidRequest, "lines must be JSON array of strings: %v", err)
					}
				}
				if p.Action == "add_page" {
					m.Pages = append(m.Pages, slidePage{ID: fmt.Sprintf("s%d", len(m.Pages)+1), Title: p.PageTitle, Lines: lines, ImagePath: p.ImagePath, ImageCaption: p.ImageCaption})
				} else {
					if p.UnitID == "" {
						return "", derr(errInvalidRequest, "unitId (page id) required for set_page")
					}
					found := false
					for i := range m.Pages {
						if m.Pages[i].ID == p.UnitID {
							m.Pages[i].Title = p.PageTitle
							m.Pages[i].Lines = lines
							m.Pages[i].ImagePath = p.ImagePath
							m.Pages[i].ImageCaption = p.ImageCaption
							found = true
						}
					}
					if !found {
						return "", derr(errUnitNotFound, "page %q not found", p.UnitID)
					}
				}
				if err := writeSlide(draft, m); err != nil {
					return "", err
				}
				return fmt.Sprintf("Updated %s (%d pages) in draft.", p.Action, len(m.Pages)), nil
			default:
				return "", derr(errInvalidRequest, "unknown action %q", p.Action)
			}
		},
	})
}

// registerExportTool 注册 office_export。
func registerExportTool(s *Server) {
	s.register(&Tool{
		Name: "office_export",
		Description: "Export an office file content to another format. Sheet (xlsx/csv/tsv) → xlsx/csv/tsv; docx → docx; pptx → pptx. " +
			"Export from trunk or a draft, to an explicit workspace path. Board export unsupported.",
		Parameters: objectSchema(map[string]interface{}{
			"file":       strPropReq("Source workspace-relative office file path."),
			"worktreeId": strProp("Export from this draft (omit for trunk)."),
			"format":     strEnum("Target format.", "xlsx", "csv", "tsv", "docx", "pptx"),
			"output":     strPropReq("Workspace-relative output path with matching extension."),
		}, []string{"file", "format", "output"}),
		Execute: func(args json.RawMessage) (string, error) {
			var p struct {
				File       string `json:"file"`
				WorktreeID string `json:"worktreeId"`
				Format     string `json:"format"`
				Output     string `json:"output"`
			}
			if err := bindArgs(args, &p); err != nil {
				return "", err
			}
			rel, err := resolveScope(pluginWS(), p.File)
			if err != nil {
				return "", err
			}
			outRel, err := resolveScope(pluginWS(), p.Output)
			if err != nil {
				return "", err
			}
			src := trunkPath(pluginWS(), rel)
			if p.WorktreeID != "" {
				src = newCtxState().store.draftPath(rel, p.WorktreeID)
			}
			if _, err := os.Stat(src); err != nil {
				return "", derr(errFileNotFound, "source %q not found", rel)
			}
			dst := trunkPath(pluginWS(), outRel)
			// 表格类转换。
			if isTableExt(rel) || p.Format == "xlsx" || p.Format == "csv" || p.Format == "tsv" {
				m, err := loadSheet(src)
				if err == nil {
					if err := saveSheet(dst, m); err != nil {
						return "", err
					}
					return fmt.Sprintf("Exported to %s (%d rows).", outRel, len(allRows(m))), nil
				}
				// 非表格格式 + format 表格：报错提示。
				return "", derr(errUnitTypeInvalid, "source is not a table (%s); table export needs xlsx/csv/tsv source", rel)
			}
			if p.Format == "docx" || strings.EqualFold(filepath.Ext(rel), ".docx") {
				m, err := loadDoc(src)
				if err != nil {
					return "", err
				}
				if err := writeDocx(dst, m); err != nil {
					return "", err
				}
				return fmt.Sprintf("Exported docx to %s (%d paragraphs).", outRel, len(m.Paras)), nil
			}
			if p.Format == "pptx" || strings.EqualFold(filepath.Ext(rel), ".pptx") {
				m, err := loadSlide(src)
				if err != nil {
					return "", err
				}
				if err := writeSlide(dst, m); err != nil {
					return "", err
				}
				return fmt.Sprintf("Exported pptx to %s (%d pages).", outRel, len(m.Pages)), nil
			}
			return "", derr(errInvalidRequest, "unsupported export combination: %s → %s", rel, p.Format)
		},
	})
}

// registerVerifyTool 注册 office_verify（结构读回校验，轻量 lint）。
func registerVerifyTool(s *Server) {
	s.register(&Tool{
		Name: "office_verify",
		Description: "Verify a draft by reading structure back (counts, well-formedness, empty checks). " +
			"Lightweight equivalent of univer_lint + inspect: checks the document opens, reports unit counts, " +
			"and flags empty cells/blank pages. Call after each batch of edits before marking ready.",
		Parameters: objectSchema(map[string]interface{}{
			"file":       strPropReq("Workspace-relative office file path."),
			"worktreeId": strPropReq("Draft worktree id to verify."),
			"strict":     strProp("If 'true', return error when structure problems found (default false)."),
		}, []string{"file", "worktreeId"}),
		Execute: func(args json.RawMessage) (string, error) {
			var p struct {
				File       string `json:"file"`
				WorktreeID string `json:"worktreeId"`
				Strict     string `json:"strict"`
			}
			if err := bindArgs(args, &p); err != nil {
				return "", err
			}
			rel, err := resolveScope(pluginWS(), p.File)
			if err != nil {
				return "", err
			}
			st := newCtxState()
			draft := st.store.draftPath(rel, p.WorktreeID)
			var findings []string
			ext := strings.ToLower(filepath.Ext(draft))
			var summary string
			switch ext {
			case ".xlsx", ".csv", ".tsv":
				m, err := loadSheet(draft)
				if err != nil {
					return "", derr(errContentBad, "draft not readable: %v", err)
				}
				total := len(allRows(m))
				summary = fmt.Sprintf("Sheet %q: %d rows, %d columns", m.Name, total, maxCols(allRows(m)))
				if total == 0 {
					findings = append(findings, "empty table (no rows)")
				}
			case ".docx":
				m, err := loadDoc(draft)
				if err != nil {
					return "", derr(errContentBad, "draft not readable: %v", err)
				}
				summary = fmt.Sprintf("Document: %d paragraphs", len(m.Paras))
				if len(m.Paras) == 0 {
					findings = append(findings, "empty document")
				}
			case ".pptx":
				m, err := loadSlide(draft)
				if err != nil {
					return "", derr(errContentBad, "draft not readable: %v", err)
				}
				summary = fmt.Sprintf("Presentation: %d pages", len(m.Pages))
				if len(m.Pages) == 0 {
					findings = append(findings, "empty presentation")
				}
				for _, pg := range m.Pages {
					if pg.Title == "" && len(pg.Lines) == 0 {
						findings = append(findings, "page "+pg.ID+" is blank")
					}
				}
			default:
				return "", derr(errUnitTypeInvalid, "unsupported format: %s", ext)
			}
			var b strings.Builder
			b.WriteString(summary + "\n")
			if len(findings) == 0 {
				b.WriteString("OK: no structural issues.\n")
			} else {
				b.WriteString("Findings:\n")
				for _, f := range findings {
					b.WriteString("  - " + f + "\n")
				}
				if p.Strict == "true" {
					return "", derr(errContentBad, "verify failed: %s", strings.Join(findings, "; "))
				}
			}
			return b.String(), nil
		},
	})
}

// ---------- 工具辅助 ----------

// register 注册工具（重名覆盖）。
func (s *Server) register(t *Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tools == nil {
		s.tools = map[string]*Tool{}
	}
	s.tools[t.Name] = t
}

// setCell 设置单元格（xlsx）。
func setCell(path, sheet, row, col, value string) error {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if sheet == "" {
		sheet = f.GetSheetName(0)
	}
	if row == "" {
		row = "1"
	}
	colName := col
	if n, err := strconvAtoi(col); err == nil && n > 0 {
		colName, err = excelize.ColumnNumberToName(n)
		if err != nil {
			return err
		}
	}
	if err := f.SetCellValue(sheet, colName+row, value); err != nil {
		return err
	}
	return f.Save()
}

// setRange 写块（xlsx，从 A1 起）。
func setRange(path, sheet string, rows [][]string) error {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if sheet == "" {
		sheet = f.GetSheetName(0)
	}
	for r, row := range rows {
		for c, cell := range row {
			cellName, _ := excelize.CoordinatesToCellName(c+1, r+1)
			if err := f.SetCellValue(sheet, cellName, cell); err != nil {
				return err
			}
		}
	}
	return f.Save()
}

func strconvAtoi(s string) (int, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

func maxCols(rows [][]string) int {
	m := 0
	for _, r := range rows {
		if len(r) > m {
			m = len(r)
		}
	}
	return m
}

func isTableExt(p string) bool {
	e := strings.ToLower(filepath.Ext(p))
	return e == ".xlsx" || e == ".csv" || e == ".tsv"
}

func appendUnique(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func removeString(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func sortedKeys(m map[string]*Tool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// registerTools 注册全部领域工具。
func registerTools(s *Server) {
	registerNewTool(s)
	registerStatusTool(s)
	registerWorktreeTool(s)
	registerUnitTool(s)
	registerInspectTool(s)
	registerExecuteTool(s)
	registerExportTool(s)
	registerVerifyTool(s)
}

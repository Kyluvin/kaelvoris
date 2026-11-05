// kaelvoris-server 远程服务中心（P-Remote 远程控制面）：
//
//	PC 端（kaelvoris-desktop）作为节点经 /ws/agent 长连接注册；
//	手机 H5（web/mobile）与管理后台（web/admin）经 /ws/client 连接，
//	查看节点/任务、发布任务、续聊会话、审批 ask。
//
// 运行：
//
//	kaelvoris-server -addr :8900 -data ~/.kaelvoris-server
//
// 首次启动自动生成 accounts.json（管理员 admin/admin123、移动端 user/user123；
// 去令牌化 2026-10：节点注册走统一账号，不再生成节点令牌）。凭据可在文件中修改，重启生效。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"kaelvoris/internal/remote"
	"kaelvoris/internal/version"
)

// accountsFile 凭据持久化结构。
//
// P-Account 统一账号体系：users 为唯一权威账号表（PC/移动/管理端同一套），
// 同一账号任意端登录 → 同一身份（admin 可用管理后台，user 仅移动端+聊天）。
// Admins/Mobiles 为旧双表（兼容字段）：加载时自动并入 users 后置空，
// 首次保存后文件中不再出现。
type accountsFile struct {
	Users       []remote.Account  `json:"users"`             // 统一账号（唯一权威）
	Admins      map[string]string `json:"admins,omitempty"`  // 旧：管理后台账号（迁移后移除）
	Mobiles     map[string]string `json:"mobiles,omitempty"` // 旧：移动端账号（迁移后移除）
	AgentTokens map[string]string `json:"agentTokens"`       // 节点令牌 token → nodeID
	ClientToken string            `json:"clientToken"`       // 移动端/管理端共享令牌（可空）
}

func main() {
	var (
		addr        = flag.String("addr", ":8900", "listen address")
		data        = flag.String("data", "", "data directory (default ~/.kaelvoris-server)")
		apiUserFlag = flag.String("api-user", "", "REST API Basic 认证用户名（默认取首个 admin 贿号）")
		apiPassFlag = flag.String("api-pass", "", "REST API Basic 认证密码")
		apiToken    = flag.String("api-token", "", "REST API 访问令牌（空 = 用 clientToken）")
		noAPIAuth   = flag.Bool("no-api-auth", false, "关闭 REST API 鉴权（仅限纯内网部署）")
		docsFlag    = flag.String("docs", "", "知识库 doc 目录（默认 exe 同目录 doc/；传 -docs= 禁用 /api/docs*）")
		sweepSec    = flag.Int("node-grace", 150, "节点失联巡检宽限期（秒）；断连超此值后自动收敛该节点 running 任务为失败")
		storeLimit  = flag.Int("store-limit", 512, "消息存储总量上限（MB，含任务/会话/去重表/生成图片）；超限按日期从旧到新清理，0 = 关闭")
	)
	flag.Parse()

	// 启动配置集中校验（config.go）：必填环境变量/配置项及格式，缺失或非法时
	// 一次性汇总缺失清单并 exit non-zero，避免运行期半初始化状态。
	if errs := validateStartup(*data, *addr, *apiUserFlag, *apiPassFlag, *noAPIAuth, *sweepSec, *storeLimit); len(errs) > 0 {
		configFatal(errs)
	}

	dir := *data
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			panic(err)
		}
		dir = filepath.Join(home, ".kaelvoris", "server")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}

	acc := loadAccounts(dir)
	hub := remote.NewHub(dir, acc.AgentTokens, acc.ClientToken, acc.Admins, acc.Mobiles, acc.Users)
	// 统一账号表变更回调：account.passwd 改密后把 UsersSnapshot 重新持久化
	// accounts.json（内存即改即生效，重启后凭据不回退）。
	hub.OnUsersChanged(func() {
		saveAccounts(dir, hub.UsersSnapshot())
	})
	// 消息存储总量上限（P-Storage，storage_limit.go）：NewHub 已默认 512MB，
	// 此处按 -store-limit 覆盖（0 = 关闭自动清理）。首轮巡检在启动 1 分钟后。
	hub.SetStoreLimit(int64(*storeLimit) << 20)

	// REST 敏感接口（/api/nodes /api/tasks）鉴权凭据：
	//   Basic 认证优先用显式 -api-user/-api-pass，否则取首个 admin 账号；
	//   token 为 clientToken（Bearer/X-Access-Token/?token= 均可）。
	//   三者全空（未配置任何账号且未显式关闭）→ 默认采用 admin/admin123 兜底，
	//   防止公网部署时敏感接口裸奔。
	apiUser, apiPass := *apiUserFlag, *apiPassFlag
	if apiUser == "" {
		if u, p := firstAdmin(acc.Users, acc.Admins); u != "" {
			apiUser, apiPass = u, p
		} else if !*noAPIAuth {
			apiUser, apiPass = "admin", "admin123"
		}
	}
	restToken := acc.ClientToken
	if *apiToken != "" {
		restToken = *apiToken
	}
	if *noAPIAuth {
		apiUser, apiPass, restToken = "", "", ""
	}

	// 静态托管：exe 同目录 web/（web/admin、web/mobile），无则仅 API。
	webDir := filepath.Join(exeDir(), "web")
	if st, err := os.Stat(filepath.Join(webDir, "admin")); err != nil || !st.IsDir() {
		webDir = ""
	}
	srv := remote.NewServer(hub, webDir)
	srv.SetRESTCredentials(apiUser, apiPass, restToken)

	// 生成图片托管（绘画链路）：图片落数据目录 images/，/img/ 公开只读；
	// 上传鉴权沿用旧节点令牌表（遗留路径，新节点注册走统一账号，不再发令牌）。
	srv.SetImageDir(filepath.Join(dir, "images"))
	srv.SetAgentTokens(acc.AgentTokens)

	// 插件市场（技能/插件/MCP 包发布与分发）：数据目录 market/。
	srv.SetMarketDir(filepath.Join(dir, "market"))

	// 节点失联巡检：断连超宽限期的节点 running 任务自动收敛 failed 并广播
	//（修复节点失联后任务卡/会话徽标永久滞留「运行中」，见 hub.go StartWatchdog）。
	hub.StartWatchdog(time.Duration(*sweepSec) * time.Second)

	// 知识库目录：默认 exe 同目录 doc/（部署目录 D:\service\kaelvoris-server\doc\，
	// 由 deploy-server.ps1 同步）；显式 -docs=（空值）禁用。
	docsDir := *docsFlag
	if docsDir == "" {
		docsDir = filepath.Join(exeDir(), "doc")
	}
	if st, err := os.Stat(docsDir); err == nil && st.IsDir() {
		srv.SetDocsDir(docsDir)
		fmt.Printf("  知识库:   %s（/api/docs* 已启用）\n", docsDir)
	} else if *docsFlag != "" {
		// 显式指定但不存在：报错提示，避免误以为功能开启。
		fmt.Printf("  知识库:   目录不存在，/api/docs* 已禁用: %s\n", docsDir)
	}

	go func() {
		if err := srv.Start(*addr); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP 服务退出", "err", err)
			os.Exit(1)
		}
	}()

	fmt.Printf("kaelvoris-server %s\n", version.Number)
	fmt.Printf("  监听:     http://0.0.0.0%s\n", *addr)
	fmt.Printf("  管理后台: http://<host>%s/admin/\n", *addr)
	fmt.Printf("  移动端:   http://<host>%s/m/\n", *addr)
	fmt.Printf("  数据目录: %s\n", dir)
	if *storeLimit > 0 {
		fmt.Printf("  存储上限: %dMB（超限按日期从旧到新清理）\n", *storeLimit)
	} else {
		fmt.Printf("  存储上限: 已关闭\n")
	}
	fmt.Printf("  节点鉴权: 统一账号（accounts.json，机器令牌链路已下线）\n")
	if apiUser != "" || restToken != "" {
		fmt.Printf("  REST 鉴权: /api/nodes /api/tasks 需凭据（%s）\n", restAuthSummary(apiUser, restToken))
	} else {
		fmt.Printf("  REST 鉴权: 已关闭（-no-api-auth，仅限内网）\n")
	}

	// 退出信号。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("正在关闭…")
	srv.Stop()
	// 持久化收尾：等已置脏数据全部落盘后停掉落盘 goroutine（防退出丢最后一批状态）。
	hub.FlushPersistence()
	hub.Stop()
	time.Sleep(200 * time.Millisecond)
}

// saveAccounts 账号文件持久化（账号表变更回调）：读当前文件保留 ClientToken，
// 注入最新账号快照后原子写回。
func saveAccounts(dir string, users []remote.Account) {
	acc2 := loadAccounts(dir)
	acc2.Users = users
	if len(acc2.Users) == 0 {
		acc2.Users = nil
	}
	if bb, err := json.MarshalIndent(acc2, "", "  "); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "accounts.json"), bb, 0o600); err != nil {
			slog.Warn("accounts.json 持久化失败", "err", err)
		}
	}
}

// firstAdmin 返回第一个 admin 角色账号（用户名+密码）；旧双表兼容（Admins 即 admin）。
func firstAdmin(users []remote.Account, legacyAdmins map[string]string) (string, string) {
	for _, a := range users {
		if a.Role == "admin" && a.User != "" && a.Pass != "" {
			return a.User, a.Pass
		}
	}
	for u, p := range legacyAdmins {
		if u != "" && p != "" {
			return u, p
		}
	}
	return "", ""
}

// restAuthSummary 鉴权方式摘要（启动日志用，不回显完整凭据）。
func restAuthSummary(user, token string) string {
	parts := make([]string, 0, 2)
	if user != "" {
		parts = append(parts, "Basic "+user)
	}
	if token != "" {
		parts = append(parts, "Bearer ***)")
	}
	return strings.Join(parts, " / ")
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// loadAccounts 加载（或首次生成）凭据文件；旧双表自动迁移为统一账号表。
func loadAccounts(dir string) accountsFile {
	path := filepath.Join(dir, "accounts.json")
	if b, err := os.ReadFile(path); err == nil {
		var a accountsFile
		if json.Unmarshal(b, &a) == nil {
			if a.AgentTokens == nil {
				a.AgentTokens = map[string]string{}
			}
			// 迁移判定：无 users 且有旧双表 → 合并迁移（admins→admin 角色，
			// mobiles→user 角色；同名账号 admin 优先），保存后旧字段移除。
			if len(a.Users) == 0 && (a.Admins != nil || a.Mobiles != nil) {
				seen := map[string]bool{}
				for u, p := range a.Admins {
					a.Users = append(a.Users, remote.Account{User: u, Pass: p, Role: "admin", DisplayName: u})
					seen[u] = true
				}
				for u, p := range a.Mobiles {
					if !seen[u] {
						a.Users = append(a.Users, remote.Account{User: u, Pass: p, Role: "user", DisplayName: u})
					}
				}
				a.Admins = nil
				a.Mobiles = nil
				if bb, err := json.MarshalIndent(a, "", "  "); err == nil {
					_ = os.WriteFile(path, bb, 0o600)
					fmt.Println("accounts.json 已迁移为统一账号体系（同一账号 PC/移动端互通）")
				}
			}
			if len(a.Users) > 0 {
				return a
			}
		}
	}
	// 首次生成默认凭据（统一账号表：admin 管理员 + user 普通用户，两端互通；
	// 去令牌化：不再生成节点令牌，PC 端注册直接用账号密码）。
	a := accountsFile{
		Users: []remote.Account{
			{User: "admin", Pass: "admin123", Role: "admin", DisplayName: "管理员"},
			{User: "user", Pass: "user123", Role: "user", DisplayName: "用户"},
		},
		AgentTokens: map[string]string{},
		ClientToken: "",
	}
	b, _ := json.MarshalIndent(a, "", "  ")
	_ = os.WriteFile(dir+"/accounts.json", b, 0o600) // 私有权限（含密码明文）
	fmt.Println("已生成默认凭据 accounts.json（统一账号 admin/admin123、user/user123，PC/移动互通）")
	return a
}

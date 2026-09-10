package main

import (
	_ "embed"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed login.html
var loginHTML string

//go:embed dashboard.html
var dashboardHTML string

const (
	upstreamBaseCN    = "https://copilot.tencent.com"
	billingBaseCN     = "https://www.codebuddy.cn"
	clientUA          = "CLI/2.63.2 CodeBuddy/2.63.2"
	originReferer     = "https://www.codebuddy.cn"
	endpointAuthState = upstreamBaseCN + "/v2/plugin/auth/state?platform=CLI"
	endpointLoginAcct = upstreamBaseCN + "/v2/plugin/login/account?state="
	endpointAuthToken = upstreamBaseCN + "/v2/plugin/auth/token?state="
)

type Config struct {
	Listen     string `json:"listen"`
	AdminKey   string `json:"admin_key"`
	APIKey     string `json:"api_key"`
	AuthDir    string `json:"auth_dir"`
	ConfigFile string `json:"config_file"`
	WB2AUrl    string `json:"wb2a_url"`
}

var (
	cfg Config
	mu  sync.Mutex
)

func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("upstream error %d: %s", resp.StatusCode, string(raw))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, err
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

type authFile struct {
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
	} `json:"auth"`
	Account struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	} `json:"account"`
}

type PackageDetail struct {
	Name        string `json:"name"`
	Remain      int64  `json:"remain"`
	Size        int64  `json:"size"`
	CycleEnd    string `json:"cycle_end"`
}

type resourcePackage struct {
	PackageName         string `json:"PackageName"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CapacitySize        int64  `json:"CapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleEndTime        string `json:"CycleEndTime"`
}

func packageRemainUsed(a resourcePackage) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

func fetchUserResource(af *authFile) (remain, used, size int64, packs int, earliestExpiry string, details []PackageDetail, err error) {
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	req, err := http.NewRequest(http.MethodPost, billingBaseCN+"/v2/billing/meter/get-user-resource", bytes.NewReader(body))
	if err != nil {
		return 0, 0, 0, 0, "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+af.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", clientUA)
	if af.Account.UID != "" {
		req.Header.Set("X-User-Id", af.Account.UID)
	}
	if af.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", af.Account.EnterpriseID)
		req.Header.Set("X-Tenant-Id", af.Account.EnterpriseID)
	}
	if af.Auth.Domain != "" {
		req.Header.Set("X-Domain", af.Auth.Domain)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, 0, "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, 0, 0, 0, "", nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					TotalDosage int64             `json:"TotalDosage"`
					Accounts    []resourcePackage `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return 0, 0, 0, 0, "", nil, err
	}
	if env.Code != 0 {
		return 0, 0, 0, 0, "", nil, fmt.Errorf("code=%d %s", env.Code, env.Msg)
	}
	for _, a := range env.Data.Response.Data.Accounts {
		r, u, s := packageRemainUsed(a)
		remain += r
		used += u
		size += s
		if r > 0 && a.CycleEndTime != "" {
			if earliestExpiry == "" || a.CycleEndTime < earliestExpiry {
				earliestExpiry = a.CycleEndTime
			}
		}
		details = append(details, PackageDetail{
			Name:     a.PackageName,
			Remain:   r,
			Size:     s,
			CycleEnd: a.CycleEndTime,
		})
	}
	packs = len(env.Data.Response.Data.Accounts)
	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	if dosage := env.Data.Response.Data.TotalDosage; dosage > size {
		size = dosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	return remain, used, size, packs, earliestExpiry, details, nil
}

func doDailyCheckin(af *authFile) (string, bool, error) {
	req, err := http.NewRequest(http.MethodPost, billingBaseCN+"/v2/billing/meter/daily-checkin", bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+af.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", clientUA)
	if af.Account.UID != "" {
		req.Header.Set("X-User-Id", af.Account.UID)
	}
	if af.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", af.Account.EnterpriseID)
		req.Header.Set("X-Tenant-Id", af.Account.EnterpriseID)
	}
	if af.Auth.Domain != "" {
		req.Header.Set("X-Domain", af.Auth.Domain)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var res struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &res)
	if res.Code == 0 {
		return "签到成功 (积分已到账)", true, nil
	}
	if strings.Contains(res.Msg, "已签到") || res.Code == 10001 {
		return "今日已签到过", true, nil
	}
	return res.Msg, false, fmt.Errorf("code=%d msg=%s", res.Code, res.Msg)
}

func checkUserSignedToday(af *authFile) bool {
	_, checked, _ := doDailyCheckin(af)
	return checked
}

func restartWb2apiContainer() {
	cmd := exec.Command("docker", "restart", "workbuddy2api")
	_ = cmd.Run()
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("wb_token")
		token := ""
		if err == nil {
			token = cookie.Value
		}
		if token == "" {
			token = r.Header.Get("X-Admin-Key")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.AdminKey)) != 1 {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func main() {
	cfg = Config{
		Listen:     "127.0.0.1:7864",
		AdminKey:   "",
		APIKey:     "",
		AuthDir:    "/opt/workbuddy2api/auths",
		ConfigFile: "/opt/workbuddy2api/config.json",
		WB2AUrl:    "http://127.0.0.1:7863",
	}
	if envKey := os.Getenv("ADMIN_KEY"); envKey != "" {
		cfg.AdminKey = envKey
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(loginHTML))
	})

	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]any{"error": "invalid request"})
			return
		}
		if subtle.ConstantTimeCompare([]byte(req.Key), []byte(cfg.AdminKey)) != 1 {
			writeJSON(w, 401, map[string]any{"error": "管理口令错误"})
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "wb_token",
			Value:    cfg.AdminKey,
			Path:     "/",
			HttpOnly: true,
			MaxAge:   86400 * 30,
			SameSite: http.SameSiteLaxMode,
		})
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:   "wb_token",
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
	}))

	// API: 概览与账号池状态
	mux.HandleFunc("GET /api/overview", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		// 1. 请求 wb2api /status 获取实时池状态
		wbReq, _ := http.NewRequest("GET", cfg.WB2AUrl+"/status", nil)
		wbReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		wbClient := &http.Client{Timeout: 5 * time.Second}
		var poolStatus struct {
			Accounts []struct {
				UID          string `json:"uid"`
				Nickname     string `json:"nickname"`
				Credits      int64  `json:"credits"`
				Cooling      bool   `json:"cooling"`
				Until        string `json:"until"`
				Disabled     bool   `json:"disabled"`
				SuccessCount int64  `json:"success_count"`
				InFlight     int    `json:"in_flight"`
				BreakerFails int    `json:"breaker_fails"`
				BreakerUntil string `json:"breaker_until"`
			} `json:"accounts"`
			Total          int `json:"total"`
			Healthy        int `json:"healthy"`
			Cooling        int `json:"cooling"`
			Disabled       int `json:"disabled"`
			InFlightFull   int `json:"in_flight_full"`
			StickySessions int `json:"sticky_sessions"`
		}
		if resp, err := wbClient.Do(wbReq); err == nil {
			_ = json.NewDecoder(resp.Body).Decode(&poolStatus)
			resp.Body.Close()
		}

		// 2. 读取当前 config.json
		var currentConfig map[string]any
		if rawCfg, err := os.ReadFile(cfg.ConfigFile); err == nil {
			_ = json.Unmarshal(rawCfg, &currentConfig)
		}

		// 3. 读 auths 凭证并并发查积分及今日签到状态
		files, _ := filepath.Glob(filepath.Join(cfg.AuthDir, "workbuddy-*.json"))
		sort.Strings(files)

		type AccountCard struct {
			UID          string          `json:"uid"`
			Nickname     string          `json:"nickname"`
			ExpiresAt    int64           `json:"expires_at"`
			Remain       int64           `json:"remain"`
			Used         int64           `json:"used"`
			Size         int64           `json:"size"`
			Packages     int             `json:"packages"`
			EarliestEnd  string          `json:"earliest_end"`
			PkgDetails   []PackageDetail `json:"pkg_details"`
			Cooling      bool            `json:"cooling"`
			Disabled     bool            `json:"disabled"`
			InFlight     int             `json:"in_flight"`
			SuccessCount int64           `json:"success_count"`
			BreakerFails int             `json:"breaker_fails"`
			StatusText   string          `json:"status_text"`
			StatusBadge  string          `json:"status_badge"`
			CheckedToday bool            `json:"checked_today"`
		}

		var (
			cards      = make([]AccountCard, 0, len(files))
			totalRem   int64
			totalUsed  int64
			totalCap   int64
			wg         sync.WaitGroup
			cardMu     sync.Mutex
		)

		poolMap := make(map[string]any)
		for _, a := range poolStatus.Accounts {
			poolMap[a.UID] = a
		}

		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var af authFile
			if err := json.Unmarshal(raw, &af); err != nil {
				continue
			}

			wg.Add(1)
			go func(file string, a authFile) {
				defer wg.Done()
				rem, used, size, packs, earliestExp, pkgDetails, _ := fetchUserResource(&a)
				isChecked := checkUserSignedToday(&a)

				card := AccountCard{
					UID:          a.Account.UID,
					Nickname:     a.Account.Nickname,
					ExpiresAt:    a.Auth.ExpiresAt,
					Remain:       rem,
					Used:         used,
					Size:         size,
					Packages:     packs,
					EarliestEnd:  earliestExp,
					PkgDetails:   pkgDetails,
					CheckedToday: isChecked,
				}
				if card.Nickname == "" {
					card.Nickname = "未命名账号"
				}

				if pRaw, ok := poolMap[a.Account.UID]; ok {
					p := pRaw.(struct {
						UID          string `json:"uid"`
						Nickname     string `json:"nickname"`
						Credits      int64  `json:"credits"`
						Cooling      bool   `json:"cooling"`
						Until        string `json:"until"`
						Disabled     bool   `json:"disabled"`
						SuccessCount int64  `json:"success_count"`
						InFlight     int    `json:"in_flight"`
						BreakerFails int    `json:"breaker_fails"`
						BreakerUntil string `json:"breaker_until"`
					})
					card.Cooling = p.Cooling
					card.Disabled = p.Disabled
					card.InFlight = p.InFlight
					card.SuccessCount = p.SuccessCount
					card.BreakerFails = p.BreakerFails
				}

				if card.Disabled {
					card.StatusText = "已失效"
					card.StatusBadge = "danger"
				} else if card.Cooling {
					card.StatusText = "冷却中"
					card.StatusBadge = "warning"
				} else {
					card.StatusText = "健康运行"
					card.StatusBadge = "success"
				}

				cardMu.Lock()
				cards = append(cards, card)
				totalRem += rem
				totalUsed += used
				totalCap += size
				cardMu.Unlock()
			}(f, af)
		}
		wg.Wait()

		sort.Slice(cards, func(i, j int) bool {
			return cards[i].UID < cards[j].UID
		})

		writeJSON(w, 200, map[string]any{
			"total_accounts":  len(cards),
			"healthy":         poolStatus.Healthy,
			"cooling":         poolStatus.Cooling,
			"disabled":        poolStatus.Disabled,
			"in_flight":       poolStatus.InFlightFull,
			"sticky_sessions": poolStatus.StickySessions,
			"total_remain":    totalRem,
			"total_used":      totalUsed,
			"total_size":      totalCap,
			"api_key":         cfg.APIKey,
			"accounts":        cards,
			"config":          currentConfig,
		})
	}))

	// API: 实时拉取容器日志
	mux.HandleFunc("GET /api/logs", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		cmd := exec.Command("docker", "logs", "--tail", "200", "workbuddy2api")
		out, err := cmd.CombinedOutput()
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": "无法拉取容器日志: " + err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"logs": string(out)})
	}))

	// API: 获取动态模型列表（支持 ?refresh=1 透传给网关，绕过其 1h 缓存取实时倍率/促销）
	mux.HandleFunc("GET /api/models", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		target := cfg.WB2AUrl + "/v1/models"
		if r.URL.Query().Get("refresh") == "1" {
			target += "?refresh=1"
		}
		req, _ := http.NewRequest("GET", target, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": "获取模型列表失败: " + err.Error()})
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
	}))

	// API: 更新多账号调度与参数政策
	mux.HandleFunc("POST /api/policy/update", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeJSON(w, 400, map[string]any{"error": "invalid json"})
			return
		}

		mu.Lock()
		defer mu.Unlock()

		raw, err := os.ReadFile(cfg.ConfigFile)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": "读取原配置文件失败"})
			return
		}
		var fullCfg map[string]any
		if err := json.Unmarshal(raw, &fullCfg); err != nil {
			writeJSON(w, 500, map[string]any{"error": "解析原配置文件失败"})
			return
		}

		for k, v := range patch {
			if vMap, ok := v.(map[string]any); ok {
				if curMap, exist := fullCfg[k].(map[string]any); exist {
					for subK, subV := range vMap {
						curMap[subK] = subV
					}
					fullCfg[k] = curMap
					continue
				}
			}
			fullCfg[k] = v
		}

		updatedBytes, err := json.MarshalIndent(fullCfg, "", "  ")
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": "序列化配置失败"})
			return
		}

		if err := os.WriteFile(cfg.ConfigFile, updatedBytes, 0644); err != nil {
			writeJSON(w, 500, map[string]any{"error": "写入配置文件失败"})
			return
		}

		go restartWb2apiContainer()
		writeJSON(w, 200, map[string]any{"ok": true, "message": "策略配置已更新并正在重启生效"})
	}))

	// API: 全员一键签到
	mux.HandleFunc("POST /api/account/checkin-all", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		files, _ := filepath.Glob(filepath.Join(cfg.AuthDir, "workbuddy-*.json"))
		var (
			okCount      int
			alreadyCount int
			failCount    int
			results      []string
		)

		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var af authFile
			if err := json.Unmarshal(raw, &af); err != nil {
				continue
			}
			msg, checked, errCheck := doDailyCheckin(&af)
			nick := af.Account.Nickname
			if nick == "" {
				nick = af.Account.UID[:8]
			}
			if errCheck == nil {
				if checked && strings.Contains(msg, "成功") {
					okCount++
					results = append(results, fmt.Sprintf("[%s] 签到成功", nick))
				} else {
					alreadyCount++
					results = append(results, fmt.Sprintf("[%s] 今日已签到", nick))
				}
			} else {
				if checked {
					alreadyCount++
					results = append(results, fmt.Sprintf("[%s] 今日已签到", nick))
				} else {
					failCount++
					results = append(results, fmt.Sprintf("[%s] 失败: %s", nick, msg))
				}
			}
		}

		summary := fmt.Sprintf("签到总数: %d (新到账: %d, 今日已签: %d, 失败: %d)", len(files), okCount, alreadyCount, failCount)
		writeJSON(w, 200, map[string]any{
			"ok":      true,
			"summary": summary,
			"details": results,
		})
	}))

	// API: 单账号签到打卡
	mux.HandleFunc("POST /api/account/checkin", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID string `json:"uid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.UID == "" {
			writeJSON(w, 400, map[string]any{"error": "UID 不能为空"})
			return
		}
		filePath := filepath.Join(cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", req.UID))
		raw, err := os.ReadFile(filePath)
		if err != nil {
			writeJSON(w, 404, map[string]any{"error": "未找到凭据文件"})
			return
		}
		var af authFile
		_ = json.Unmarshal(raw, &af)
		res, _, err := doDailyCheckin(&af)
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": true, "result": res})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "result": res})
	}))

	// API: OAuth 获取授权链接
	mux.HandleFunc("POST /api/oauth/start", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Timeout: 15 * time.Second, Jar: jar}
		data, _, err := doJSON(client, http.MethodPost, endpointAuthState, nil, bytes.NewReader([]byte("{}")))
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": "获取授权 URL 失败: " + err.Error()})
			return
		}
		var st struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		}
		_ = json.Unmarshal(data, &st)
		writeJSON(w, 200, st)
	}))

	// API: OAuth 轮询换取 Token
	mux.HandleFunc("POST /api/oauth/poll", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.State == "" {
			writeJSON(w, 400, map[string]any{"error": "state 不能为空"})
			return
		}
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Timeout: 15 * time.Second, Jar: jar}

		tokRaw, _, errTok := doJSON(client, http.MethodGet, endpointAuthToken+req.State, nil, nil)
		if errTok != nil {
			writeJSON(w, 200, map[string]any{"status": "pending", "message": "等待用户在浏览器中完成登录..."})
			return
		}
		var tok struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		}
		if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
			writeJSON(w, 200, map[string]any{"status": "pending", "message": "尚未收到授权 Token"})
			return
		}

		var acct struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		acctHeaders := func(r *http.Request) {
			commonHeaders(r)
			r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		}
		if acctRaw, _, errAcct := doJSON(client, http.MethodGet, endpointLoginAcct+req.State, acctHeaders, nil); errAcct == nil {
			_ = json.Unmarshal(acctRaw, &acct)
		}

		if acct.UID == "" {
			writeJSON(w, 500, map[string]any{"error": "未能获取到用户 UID"})
			return
		}

		expiresAt := time.Now().Unix() + tok.ExpiresIn
		afData := authFile{}
		afData.Account.UID = acct.UID
		afData.Account.EnterpriseID = acct.EnterpriseID
		afData.Account.Nickname = acct.Nickname
		afData.Auth.AccessToken = tok.AccessToken
		afData.Auth.RefreshToken = tok.RefreshToken
		afData.Auth.ExpiresAt = expiresAt
		afData.Auth.Domain = tok.Domain

		mu.Lock()
		os.MkdirAll(cfg.AuthDir, 0755)
		savePath := filepath.Join(cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", acct.UID))
		rawBytes, _ := json.MarshalIndent(afData, "", "  ")
		_ = os.WriteFile(savePath, rawBytes, 0600)
		_ = exec.Command("chown", "10001:10001", savePath).Run()
		_ = exec.Command("chmod", "600", savePath).Run()
		mu.Unlock()

		_, _, _ = doDailyCheckin(&afData)
		go restartWb2apiContainer()

		writeJSON(w, 200, map[string]any{
			"status":   "success",
			"uid":      acct.UID,
			"nickname": acct.Nickname,
			"message":  "授权成功，账号已加入池中并自动重启生效！",
		})
	}))

	// API: 删除账号
	mux.HandleFunc("POST /api/account/delete", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID string `json:"uid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.UID == "" {
			writeJSON(w, 400, map[string]any{"error": "UID 不能为空"})
			return
		}
		filePath := filepath.Join(cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", req.UID))
		_ = os.Remove(filePath)
		go restartWb2apiContainer()
		writeJSON(w, 200, map[string]any{"ok": true, "message": "账号已删除"})
	}))

	// API: 重启容器
	mux.HandleFunc("POST /api/system/restart", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		go restartWb2apiContainer()
		writeJSON(w, 200, map[string]any{"ok": true, "message": "正在重启 workbuddy2api 容器..."})
	}))

	// API: 在线模型对话测试
	mux.HandleFunc("POST /api/chat/test", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model   string `json:"model"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "" {
			req.Model = "deepseek-v4.1-flash"
		}
		if req.Message == "" {
			req.Message = "你好"
		}

		payload, _ := json.Marshal(map[string]any{
			"model": req.Model,
			"messages": []map[string]string{
				{"role": "user", "content": req.Message},
			},
			"stream": false,
		})

		chatReq, _ := http.NewRequest("POST", cfg.WB2AUrl+"/v1/chat/completions", bytes.NewReader(payload))
		chatReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		chatReq.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 45 * time.Second}
		resp, err := client.Do(chatReq)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": "请求网关超时或失败: " + err.Error()})
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
	}))

	
	// API: 每日 Token 与消耗报表（全维度解析）
	mux.HandleFunc("GET /api/daily-report", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		cmd := exec.Command("docker", "logs", "--timestamps", "workbuddy2api")
		out, err := cmd.CombinedOutput()
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": "读取日志失败: " + err.Error()})
			return
		}

		type DayStat struct {
			Date         string            `json:"date"`
			Requests     int               `json:"requests"`
			SuccessReqs  int               `json:"success_reqs"`
			SuccessRate  string            `json:"success_rate"`
			RateNum      float64           `json:"rate_num"`
			PromptTokens int64             `json:"prompt_tokens"`
			CompTokens   int64             `json:"comp_tokens"`
			TotalTokens  int64             `json:"total_tokens"`
			HitTokens    int64             `json:"hit_tokens"`
			CacheRate    string            `json:"cache_rate"`
			CacheRateNum float64           `json:"cache_rate_num"`
			CostCredits  float64           `json:"cost_credits"`
			TotalSpeed   float64           `json:"-"`
			SpeedCount   int               `json:"-"`
			AvgSpeed     float64           `json:"avg_speed"`
			TotalTtfb    int64             `json:"-"`
			TtfbCount    int               `json:"-"`
			AvgTtfb      int64             `json:"avg_ttfb"`
			Models       map[string]int    `json:"models"`
		}

		statsMap := make(map[string]*DayStat)
		lines := strings.Split(string(out), "\n")
		// 新日志格式示例: 2026-09-10T08:23:04Z | #001 | 16:23:04 | deepseek-v4 | stream | 200 | uid=xxxxxxxx | TTFB=889ms | in=8 | out=6 | hit=0 | hit_rate=0.0% | cr=0.00 | 5.8tok/s | total=1.0s |
		// 老日志兼容: 2026-09-10T07:44:46Z | #001 | 15:44:46 | deepseek-v4 | stream | 200 | uid=xxxxxxxx | TTFB=1535ms | tok=178 | 71.5tok/s | total=2.5s |
		for _, line := range lines {
			if !strings.Contains(line, "|") {
				continue
			}
			parts := strings.Split(line, "|")
			if len(parts) < 9 {
				continue
			}

			dateStr := time.Now().Format("2006-01-02")
			if len(line) >= 10 && line[4] == '-' && line[7] == '-' {
				dateStr = line[:10]
			}

			stat, exists := statsMap[dateStr]
			if !exists {
				stat = &DayStat{
					Date:   dateStr,
					Models: make(map[string]int),
				}
				statsMap[dateStr] = stat
			}

			stat.Requests++
			model := strings.TrimSpace(parts[3])
			if model != "" && model != "-" {
				stat.Models[model]++
			}

			statusCode := strings.TrimSpace(parts[5])
			if statusCode == "200" {
				stat.SuccessReqs++
			}

			// 提取各字段
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if strings.HasPrefix(p, "TTFB=") {
					var ttfbMs int64
					if _, err := fmt.Sscanf(strings.TrimSuffix(strings.TrimPrefix(p, "TTFB="), "ms"), "%d", &ttfbMs); err == nil && ttfbMs > 0 {
						stat.TotalTtfb += ttfbMs
						stat.TtfbCount++
					}
				} else if strings.HasPrefix(p, "in=") {
					var inTok int64
					if _, err := fmt.Sscanf(strings.TrimPrefix(p, "in="), "%d", &inTok); err == nil {
						stat.PromptTokens += inTok
					}
				} else if strings.HasPrefix(p, "out=") {
					var outTok int64
					if _, err := fmt.Sscanf(strings.TrimPrefix(p, "out="), "%d", &outTok); err == nil {
						stat.CompTokens += outTok
					}
				} else if strings.HasPrefix(p, "hit=") {
					var hitTok int64
					if _, err := fmt.Sscanf(strings.TrimPrefix(p, "hit="), "%d", &hitTok); err == nil {
						stat.HitTokens += hitTok
					}
				} else if strings.HasPrefix(p, "cr=") {
					var cr float64
					if _, err := fmt.Sscanf(strings.TrimPrefix(p, "cr="), "%f", &cr); err == nil {
						stat.CostCredits += cr
					}
				} else if strings.HasPrefix(p, "tok=") { // 兼容旧版日志
					var oldTok int64
					if _, err := fmt.Sscanf(strings.TrimPrefix(p, "tok="), "%d", &oldTok); err == nil {
						stat.CompTokens += oldTok
					}
				} else if strings.HasSuffix(p, "tok/s") {
					var speed float64
					if _, err := fmt.Sscanf(strings.TrimSuffix(p, "tok/s"), "%f", &speed); err == nil && speed > 0 {
						stat.TotalSpeed += speed
						stat.SpeedCount++
					}
				}
			}
		}

		var resultList []*DayStat
		todayStr := time.Now().Format("2006-01-02")
		var todayStat *DayStat

		for _, s := range statsMap {
			if s.Requests > 0 {
				s.RateNum = float64(s.SuccessReqs) * 100.0 / float64(s.Requests)
				s.SuccessRate = fmt.Sprintf("%.1f%%", s.RateNum)
			} else {
				s.SuccessRate = "100%"
				s.RateNum = 100
			}
			s.TotalTokens = s.PromptTokens + s.CompTokens
			if s.PromptTokens > 0 {
				s.CacheRateNum = float64(s.HitTokens) * 100.0 / float64(s.PromptTokens)
				s.CacheRate = fmt.Sprintf("%.1f%%", s.CacheRateNum)
			} else {
				s.CacheRate = "0.0%"
			}
			if s.SpeedCount > 0 {
				s.AvgSpeed = float64(int(s.TotalSpeed*10/float64(s.SpeedCount))) / 10.0
			}
			if s.TtfbCount > 0 {
				s.AvgTtfb = s.TotalTtfb / int64(s.TtfbCount)
			}
			s.CostCredits = float64(int(s.CostCredits*100)) / 100.0
			resultList = append(resultList, s)
			if s.Date == todayStr {
				todayStat = s
			}
		}

		sort.Slice(resultList, func(i, j int) bool {
			return resultList[i].Date > resultList[j].Date
		})

		if todayStat == nil {
			todayStat = &DayStat{
				Date:        todayStr,
				SuccessRate: "100%",
				CacheRate:   "0.0%",
				Models:      make(map[string]int),
			}
		}

		// 日志口径只看得到「上游逐次返回的 credit」；配额真相只在官方计费接口。
		// 这里实时拉一次官方口径，单独返回给前端做对账，避免把两种口径混为「官方扣减」。
		var (
			offRemain int64
			offUsed   int64
			offSize   int64
			offMu     sync.Mutex
			offWg     sync.WaitGroup
		)
		if afiles, gerr := filepath.Glob(filepath.Join(cfg.AuthDir, "workbuddy-*.json")); gerr == nil {
			for _, f := range afiles {
				raw, rerr := os.ReadFile(f)
				if rerr != nil {
					continue
				}
				var af authFile
				if jerr := json.Unmarshal(raw, &af); jerr != nil {
					continue
				}
				offWg.Add(1)
				go func(a authFile) {
					defer offWg.Done()
					r, u, s, _, _, _, ferr := fetchUserResource(&a)
					if ferr != nil {
						return
					}
					offMu.Lock()
					offRemain += r
					offUsed += u
					offSize += s
					offMu.Unlock()
				}(af)
			}
			offWg.Wait()
		}

		var logCreditTotal float64
		for _, s := range resultList {
			logCreditTotal += s.CostCredits
		}

		writeJSON(w, 200, map[string]any{
			"today": todayStat,
			"daily": resultList,
			"official": map[string]any{
				"remain": offRemain,
				"used":   offUsed,
				"size":   offSize,
			},
			"log_credit_total": logCreditTotal,
		})
	}))

	log.Printf("WorkBuddy UI v2 starting on %s", cfg.Listen)
	if err := http.ListenAndServe(cfg.Listen, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

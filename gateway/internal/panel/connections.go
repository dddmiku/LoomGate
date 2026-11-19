package panel

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/authflow"
	"workbuddy2api/internal/providers/autoclaw"
	"workbuddy2api/internal/providers/doubao"
	"workbuddy2api/internal/upstream/trae"
)

type connections struct {
	once          sync.Once
	service       *authflow.Service
	vault         *accountvault.Vault
	providerVault *accountvault.Vault
	// providerVaultOwned is false when the handle was handed in by the
	// inference layer. Closing a borrowed vault would break live model calls,
	// so only a vault this package opened may be closed here.
	providerVaultOwned bool
	err                error
}

// doubaoPassportSave persists the session the passport produced. The profile
// already carries the login-issued Query/Headers, so it is written verbatim.
func doubaoPassportSave(s *Server) func(context.Context, string, doubao.Stored) error {
	return func(ctx context.Context, id string, profile doubao.Stored) error {
		if s.connections.providerVault == nil {
			return errors.New("provider_vault_unavailable")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b, err := json.Marshal(profile)
		if err != nil {
			return err
		}
		defer func() {
			for i := range b {
				b[i] = 0
			}
		}()
		var revision uint64
		rows, err := s.connections.providerVault.List()
		if err != nil {
			return err
		}
		for _, a := range rows {
			if a.ID == id {
				if a.Provider != "doubao" {
					return errors.New("account_provider_conflict")
				}
				revision = a.Revision
			}
		}
		_, err = s.connections.providerVault.Put(accountvault.Account{ID: id, Provider: "doubao", Enabled: true}, b, revision)
		return err
	}
}

// autoclawSaveProfile returns the callback that persists a verified AutoClaw
// login into the provider vault, so the account is callable immediately.
func autoclawSaveProfile(s *Server) func(context.Context, []byte) (string, error) {
	return func(ctx context.Context, profile []byte) (string, error) {
		if s.connections.providerVault == nil {
			return "", errors.New("provider_vault_unavailable")
		}
		return autoclaw.SaveProfile(s.connections.providerVault)(ctx, profile)
	}
}

func (s *Server) connectionsService() (*authflow.Service, error) {
	s.connections.once.Do(func() {
		v, err := accountvault.Open(filepath.Join(s.opts.AdminDir, "connection-vault"))
		if err != nil {
			s.connections.err = errors.New("authentication_storage_unavailable")
			return
		}
		drivers := map[string]authflow.Driver{
			"workbuddy": &legacyConnection{s: s, provider: "workbuddy"},
			"trae":      &legacyConnection{s: s, provider: "trae"},
			// AutoClaw logs in with a phone number + SMS code against its own
			// upstream. No token import and no desktop client are required.
			"autoclaw": &autoclaw.LoginDriver{Save: autoclawSaveProfile(s)},
			// Doubao logs in through its own passport: QR scan or phone + SMS.
			// Both produce a real session Cookie; no packet capture is needed.
			"doubao": &doubao.Passport{Save: doubaoPassportSave(s)},
		}
		for id, d := range s.opts.ConnectionDrivers {
			drivers[id] = d
		}
		service, err := authflow.Open(authflow.Options{Vault: v, Drivers: drivers})
		if err != nil {
			v.Close()
			s.connections.err = err
			return
		}
		s.connections.vault = v
		s.connections.service = service
		if s.opts.ProviderVault != nil {
			// The inference layer already owns this vault's exclusive lock.
			// Sharing the handle is the only correct option: a second Open on
			// the same directory returns vault_busy.
			s.connections.providerVault = s.opts.ProviderVault
		} else if s.opts.ProviderVaultDir != "" {
			if pv, err := accountvault.Open(s.opts.ProviderVaultDir); err == nil {
				s.connections.providerVault = pv
				s.connections.providerVaultOwned = true
			} else {
				log.Printf("WARN: [panel] 打开 provider 账号库失败（登录无法保存账号）：%v", err)
			}
		}
	})
	return s.connections.service, s.connections.err
}

// Close releases the optional authentication vault; it does not change account data.
func (s *Server) Close() error {
	if s.connections.providerVaultOwned && s.connections.providerVault != nil {
		_ = s.connections.providerVault.Close()
	}
	if s.connections.vault != nil {
		return s.connections.vault.Close()
	}
	return nil
}
func (s *Server) serveConnections(w http.ResponseWriter, r *http.Request) {
	service, err := s.connectionsService()
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "authentication_storage_unavailable"})
		return
	}
	forward := r.Clone(r.Context())
	forward.URL.Path = s.stripPrefix(r.URL.Path)
	service.Route(w, forward)
}

type legacyConnection struct {
	s        *Server
	provider string
	client   *http.Client
}
type legacyState struct {
	LoginID     string         `json:"login_id,omitempty"`
	URL         string         `json:"url,omitempty"`
	Credentials map[string]any `json:"credentials,omitempty"`
}

func (*legacyState) String() string { return "legacyState[redacted]" }
func (d *legacyConnection) Methods() []authflow.Method {
	if d.provider == "trae" {
		return []authflow.Method{{ID: "browser", Name: "官方网页授权", Available: true}}
	}
	return []authflow.Method{{ID: "qr", Name: "官方授权 / 扫码", Available: true}}
}
func safeLoginURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.User == nil && (u.Hostname() == "www.workbuddy.ai" || u.Hostname() == "workbuddy.ai" || u.Hostname() == "copilot.tencent.com" || strings.HasSuffix(u.Hostname(), ".codebuddy.cn") || u.Hostname() == "www.codebuddy.cn" || strings.HasSuffix(u.Hostname(), ".workbuddy.ai") || u.Hostname() == "www.trae.cn" || u.Hostname() == "www.trae.ai" || u.Hostname() == "www.trae.com.cn")
}
func (d *legacyConnection) Start(ctx context.Context, r authflow.Request) (authflow.Result, error) {
	if d.provider == "workbuddy" {
		return d.startWorkbuddy(ctx, r)
	}
	if d.provider != "trae" {
		return authflow.Result{}, authflow.ErrInvalid
	}
	data := d.s.traeLoginStart()
	if !jsonBool(data["ok"], false) {
		return authflow.Result{Status: "failed", ErrorCode: "login_start_failed", Notice: "无法开始官方授权，请检查服务状态。"}, nil
	}
	u := jsonString(data["url"])
	if !safeLoginURL(u) {
		return authflow.Result{Status: "failed", ErrorCode: "login_url_rejected"}, nil
	}
	b, _ := json.Marshal(legacyState{LoginID: jsonString(data["login_id"]), URL: u})
	return authflow.Result{Status: "waiting_authorization", ActionURL: u, Secret: b, Notice: "请在官方授权页面完成登录。"}, nil
}
func (d *legacyConnection) Step(ctx context.Context, f authflow.Flow, action string, input authflow.Input) (authflow.Result, error) {
	var st legacyState
	if json.Unmarshal(f.Secret, &st) != nil {
		return authflow.Result{}, authflow.ErrInvalid
	}
	if action == "poll" && d.provider == "workbuddy" {
		return d.pollWorkbuddy(ctx, f, st)
	}
	if action == "callback" && d.provider == "trae" {
		creds, err := connectionTraeCallback(ctx, input.Callback)
		if err != nil {
			return authflow.Result{Status: "waiting_authorization", ActionURL: st.URL, ErrorCode: "callback_invalid", Notice: "授权回调无效，请复制官方授权完成后的完整回调地址，仅提交到此本机表单。"}, nil
		}
		if !uidRe.MatchString(jsonString(creds["uid"])) {
			return authflow.Result{}, authflow.ErrInvalid
		}
		info, err := trae.GetUserInfo(ctx, &auth.Auth{UID: jsonString(creds["uid"]), AccessToken: jsonString(creds["access_token"]), Domain: "api.trae.cn"})
		if err != nil || info.UserID == "" || info.UserID != jsonString(creds["uid"]) {
			return authflow.Result{Status: "manual_action", ErrorCode: "account_identity_not_verified", Notice: "官方账号信息核验未通过，未保存凭据。"}, nil
		}
		creds["nickname"] = info.ScreenName
		st.Credentials = creds
		b, _ := json.Marshal(st)
		return authflow.Result{Status: "verified", Secret: b, Notice: "官方账号身份已核验，等待保存。"}, nil
	}
	if action == "commit" {
		if st.Credentials == nil {
			return authflow.Result{}, authflow.ErrInvalid
		}
		if f.AccountID != "" && f.AccountID != jsonString(st.Credentials["uid"]) {
			return authflow.Result{Status: "manual_action", ErrorCode: "account_identity_mismatch", Notice: "登录账号与所选账号不一致，未覆盖原账号。"}, nil
		}
		d.s.lock.Lock()
		defer d.s.lock.Unlock()
		var summary map[string]any
		var err error
		if d.provider == "trae" {
			expires := TraeNormalizeExpire(st.Credentials["token_expire_at"])
			if expires <= d.s.now().Unix() {
				return authflow.Result{Status: "manual_action", ErrorCode: "session_expiry_missing", Notice: "授权结果缺少有效期，请重新完成官方授权。"}, nil
			}
			uid := jsonString(st.Credentials["uid"])
			doc := map[string]any{"account": map[string]any{"uid": uid, "nickname": jsonString(st.Credentials["nickname"])}, "auth": map[string]any{"accessToken": jsonString(st.Credentials["access_token"]), "refreshToken": jsonString(st.Credentials["refresh_token"]), "expiresAt": expires, "domain": "api.trae.cn", "realm": "trae"}}
			err = d.s.writeAuthDocument(filepath.Join(d.s.opts.AuthDir, "workbuddy-trae-"+uid+".json"), doc, ".workbuddy-trae-")
			summary = map[string]any{"uid": uid}
		} else {
			remaining := jsonInt64(st.Credentials["verified_expires_at"]) - d.s.now().Unix()
			if remaining <= 0 {
				return authflow.Result{Status: "expired", ErrorCode: "session_expired_before_save"}, nil
			}
			st.Credentials["expires_in"] = remaining
			summary, err = d.s.writeAuthFile(st.Credentials)
		}
		if err != nil {
			return authflow.Result{}, errors.New("account_save_failed")
		}
		uid := jsonString(summary["uid"])
		if ok, _ := d.s.reloadAccounts(ctx); !ok {
			return authflow.Result{Status: "manual_action", AccountID: uid, ErrorCode: "account_reload_required", Notice: "账号已保存，但网关加载未确认。请检查服务状态。"}, nil
		}
		return authflow.Result{Status: "succeeded", AccountID: uid, Notice: "账号已保存并重新加载。"}, nil
	}
	return authflow.Result{}, authflow.ErrInvalid
}

// Existing callback formats remain supported, but exchange requests use the
// official host and the operation deadline, never a host supplied in a callback.
func connectionTraeCallback(ctx context.Context, raw string) (map[string]any, error) {
	query := map[string]string{}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" || u.Host != "127.0.0.1:8765" || u.Path != "/authorize" || u.User != nil {
			return nil, ErrTraeUnparseable
		}
		query = traeParseQuery(u.RawQuery)
	} else {
		query = traeParseQuery(strings.TrimPrefix(raw, "?"))
		if !traeHasCredentialParam(query) && traeLooksLikeBareToken(raw) {
			query = map[string]string{"refreshToken": raw}
		}
	}
	jwt := traeJSONLoadsSafe(traeQueryValue(query, "userJwt", "user_jwt"))
	token := traeFirstString(jwt, "Token", "token")
	if token == "" {
		refresh := traeFirstString(jwt, "RefreshToken", "refreshToken")
		if refresh == "" {
			refresh = traeQueryValue(query, "refreshToken", "refresh_token", "data")
		}
		if refresh == "" {
			return nil, ErrTraeNoUserJWT
		}
		exchanged, err := TraeExchangeRefreshToken(ctx, refresh, TraeClientID, TraeExchangeHost)
		if err != nil {
			return nil, errors.New("official_exchange_failed")
		}
		token = traePyStr(exchanged["token"])
		if token == "" {
			return nil, ErrTraeNoUserJWT
		}
		b, _ := json.Marshal(map[string]any{"Token": token, "RefreshToken": exchanged["refreshToken"], "TokenExpireAt": exchanged["tokenExpireAt"]})
		query["userJwt"] = string(b)
		info, _ := json.Marshal(exchanged["userInfo"])
		query["userInfo"] = string(info)
	}
	query["host"] = TraeExchangeHost
	return traeCredentialsFromQuery(query)
}

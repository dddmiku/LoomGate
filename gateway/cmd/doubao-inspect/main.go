// doubao-inspect performs bounded, read-only protocol verification using a
// private official-client request profile. It never emits profile contents.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
	"workbuddy2api/internal/accountvault"
	"workbuddy2api/internal/providers/doubao"
)

type source struct{ profile doubao.Session }

func (s source) Session(context.Context, string) (doubao.Session, error) { return s.profile, nil }
func run() error {
	path := flag.String("profile", "", "private official request profile")
	vaultPath := flag.String("import-vault", "", "optional local encrypted vault for this validated session")
	accountID := flag.String("account", "", "local account alias for first import")
	flag.Parse()
	b, err := os.ReadFile(*path)
	if err != nil {
		return fmt.Errorf("profile_unreadable")
	}
	var p struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"post_data"`
	}
	if json.Unmarshal(b, &p) != nil {
		return fmt.Errorf("profile_invalid")
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Hostname() != "www.doubao.com" {
		return fmt.Errorf("profile_origin_invalid")
	}
	var body struct {
		BotID string `json:"bot_id"`
	}
	if json.Unmarshal([]byte(p.Body), &body) != nil {
		return fmt.Errorf("profile_body_invalid")
	}
	headers := http.Header{}
	for k, v := range p.Headers {
		headers.Set(k, v)
	}
	hash := sha256.Sum256(b)
	client := doubao.New(source{doubao.Session{Query: u.Query(), Headers: headers, BotID: body.BotID, Revision: hex.EncodeToString(hash[:8])}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	models, err := client.Catalog(ctx, "local-verification", true)
	if err != nil {
		return err
	}
	activities, err := client.Activities(ctx, "local-verification")
	if err != nil {
		return err
	}
	checked := false
	for _, task := range activities.Tasks {
		if task.Daily != nil {
			checked = task.Daily.Checked
		}
	}
	if *vaultPath != "" {
		v, err := accountvault.Open(*vaultPath)
		if err != nil {
			return err
		}
		defer v.Close()
		secret, err := json.Marshal(doubao.StoredProfile{Query: u.Query(), Headers: headers, BotID: body.BotID})
		if err != nil {
			return fmt.Errorf("profile_encode_failed")
		}
		defer func() {
			for i := range secret {
				secret[i] = 0
			}
		}()
		if _, err = v.Put(accountvault.Account{ID: *accountID, Provider: "doubao", Enabled: true}, secret, 0); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"model_count": len(models.Models), "task_count": len(activities.Tasks), "milestone_count": len(activities.Milestones), "total_reward": activities.TotalAmount, "checked_in_today": checked, "catalog_stale": models.Stale, "mutations": 0})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

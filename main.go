package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/unraid/apprise-go"
)

type Voicemail struct {
	ID        string `json:"id"`
	Caller    string `json:"caller"`
	Timestamp string `json:"timestamp"`
	AudioURL  string `json:"audio_url"`
}

// Config holds everything read from the environment. Only the Ooma
// credentials are required; notifications are best-effort and simply
// don't fire if the Apprise settings are left unset.
type Config struct {
	OomaURL  string // e.g. https://my.ooma.com/login — the host is reused for every other Ooma endpoint
	OomaUser string
	OomaPass string

	// One or more Apprise URLs (https://github.com/caronc/apprise/wiki) —
	// e.g. mailto://user:pass@smtp.example.com, discord://webhook_id/webhook_token.
	// Sent via github.com/unraid/apprise-go in-process; no separate service
	// to run. Leave empty to disable notifications (voicemails are still
	// downloaded either way).
	AppriseURLs []string

	CheckInterval time.Duration
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		OomaURL:     os.Getenv("OOMA_URL"),
		OomaUser:    os.Getenv("OOMA_USER"),
		OomaPass:    os.Getenv("OOMA_PASS"),
		AppriseURLs: splitAndTrim(os.Getenv("APPRISE_URLS"), ","),
	}
	if cfg.OomaURL == "" || cfg.OomaUser == "" || cfg.OomaPass == "" {
		return nil, fmt.Errorf("missing OOMA credentials; set OOMA_URL, OOMA_USER, OOMA_PASS")
	}
	if len(cfg.AppriseURLs) == 0 {
		slog.Warn("APPRISE_URLS not set; voicemails will be downloaded but no notification will be sent")
	}

	interval := 15 * time.Minute
	if raw := os.Getenv("CHECK_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid CHECK_INTERVAL %q: %w", raw, err)
		}
		interval = d
	}
	cfg.CheckInterval = interval

	return cfg, nil
}

// splitAndTrim splits s on sep, trims whitespace from each piece, and drops
// empty results — e.g. for parsing a comma-separated env var.
func splitAndTrim(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// oomaBaseURL returns the scheme+host of the configured OOMA_URL, so every
// other endpoint (the voicemail page, the get_link AJAX call) is derived
// from it instead of a hardcoded my.ooma.com.
func oomaBaseURL(loginURL string) (string, error) {
	u, err := url.Parse(loginURL)
	if err != nil {
		return "", fmt.Errorf("invalid OOMA_URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("OOMA_URL must be an absolute URL, got %q", loginURL)
	}
	return u.Scheme + "://" + u.Host, nil
}

func main() {
	stateFile := flag.String("state-file", "voicemails.json", "JSON file to track seen voicemails")
	mp3Dir := flag.String("mp3-dir", "./voicemails", "Directory to store MP3 files")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(*mp3Dir, 0o755); err != nil {
		slog.Error("failed creating mp3 dir", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("starting ooma-voicemail", "interval", cfg.CheckInterval.String(), "state_file", *stateFile, "mp3_dir", *mp3Dir)

	runCheck(cfg, *stateFile, *mp3Dir)

	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("shutdown signal received, exiting")
			return
		case <-ticker.C:
			runCheck(cfg, *stateFile, *mp3Dir)
		}
	}
}

// runCheck performs one full pass: log in, fetch the voicemail page, download
// anything new, notify, and persist the updated seen-state. Errors within a
// pass are logged, not fatal — a transient failure shouldn't kill the daemon,
// the next tick just tries again.
func runCheck(cfg *Config, stateFile, mp3Dir string) {
	base, err := oomaBaseURL(cfg.OomaURL)
	if err != nil {
		slog.Error("bad OOMA_URL", "err", err)
		return
	}
	voicemailPageURL := base + "/voicemail"

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	if err := loginOoma(client, cfg.OomaURL, cfg.OomaUser, cfg.OomaPass); err != nil {
		slog.Error("ooma login failed", "err", err)
		// continue — the page fetch may still work if cookies persisted elsewhere
	} else {
		slog.Info("ooma login succeeded")
	}

	// Ask the server for 100 voicemails per page instead of its paginated default.
	if u, errp := url.Parse(voicemailPageURL); errp == nil {
		client.Jar.SetCookies(u, []*http.Cookie{{Name: "voicemails_per_page", Value: "100", Path: "/", Domain: u.Host}})
	}

	resp, err := client.Get(voicemailPageURL)
	if err != nil {
		slog.Error("failed fetching voicemail page", "err", err)
		return
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		slog.Error("failed parsing voicemail page", "err", err)
		return
	}

	seen := loadSeen(stateFile)

	newCount := 0
	var latestID, latestCaller, latestTimestamp, latestAudioURL string
	doc.Find("tr[data-id]").Each(func(i int, s *goquery.Selection) {
		id, _ := s.Attr("data-id")
		if id == "" {
			return
		}

		caller := extractCaller(s)
		timestamp := extractTimestamp(s)
		audioURL := extractAudioURL(s)

		if i == 0 {
			latestID, latestCaller, latestTimestamp, latestAudioURL = id, caller, timestamp, audioURL
		}

		if _, exists := seen[id]; exists {
			return
		}

		slog.Info("new voicemail", "id", id, "caller", caller, "timestamp", timestamp)
		if resolved, err := processVoicemail(client, cfg, base, mp3Dir, id, caller, timestamp, audioURL); err != nil {
			slog.Error("failed processing voicemail", "id", id, "err", err)
		} else {
			audioURL = resolved
		}

		seen[id] = Voicemail{ID: id, Caller: caller, Timestamp: timestamp, AudioURL: audioURL}
		newCount++
	})

	if newCount == 0 {
		slog.Info("no new voicemails found")
		if latestID != "" {
			if latestAudioURL == "" {
				csrf := fetchCSRFToken(client, voicemailPageURL)
				if link, err := getDownloadLink(client, voicemailPageURL, latestID, "INBOX", csrf); err == nil {
					latestAudioURL = link
				}
			}
			slog.Info("latest voicemail from site", "id", latestID, "caller", latestCaller, "timestamp", latestTimestamp, "url", latestAudioURL)
		} else {
			slog.Info("could not determine latest voicemail from page")
		}
	}

	if err := saveSeen(stateFile, seen); err != nil {
		slog.Error("failed saving state", "err", err)
	}
}

// extractCaller pulls the caller name/number out of a voicemail row.
// Caller name/number lives in span.v_cont_name (data-number or data-name).
func extractCaller(s *goquery.Selection) string {
	callerSel := s.Find("span.v_cont_name")
	if caller, ok := callerSel.Attr("data-number"); ok {
		return caller
	}
	if caller, ok := callerSel.Attr("data-name"); ok {
		return caller
	}
	return strings.TrimSpace(callerSel.Text())
}

// extractTimestamp prefers the data-date attribute (e.g. 11_26_2024_09_20_PM)
// over the visible column text, since the attribute is unambiguous while the
// rendered text is locale/format dependent.
func extractTimestamp(s *goquery.Selection) string {
	if raw, ok := s.Find("td[data-date]").Attr("data-date"); ok {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			if t, err := time.ParseInLocation("01_02_2006_03_04_PM", raw, time.Local); err == nil {
				return t.Format("2006-01-02 15:04")
			}
			// If parsing fails, keep the raw attribute for traceability.
			return raw
		}
	}
	return strings.TrimSpace(s.Find("td").Eq(4).Text())
}

// extractAudioURL tries several places Ooma has been observed to put the
// download link for a voicemail row: an <audio> tag, an <a> wrapping a
// download icon, any <a href> ending in .mp3, or a data-url/data-href
// attribute on the play/download control. Returns "" if none match — the
// caller falls back to the get_link AJAX endpoint in that case.
func extractAudioURL(s *goquery.Selection) string {
	if src, ok := s.Find("audio").Attr("src"); ok && src != "" {
		return src
	}

	var audioURL string
	s.Find("a").EachWithBreak(func(j int, a *goquery.Selection) bool {
		href, ok := a.Attr("href")
		if !ok || href == "" {
			return true
		}
		if a.Find("i.fa-download, i.fa.fa-download").Length() > 0 || strings.HasSuffix(strings.ToLower(href), ".mp3") {
			audioURL = href
			return false
		}
		return true
	})
	if audioURL != "" {
		return audioURL
	}

	if v, ok := s.Find(".play, .player, i.fa-download").Attr("data-url"); ok {
		return v
	}
	if v, ok := s.Find(".play, .player, i.fa-download").Attr("data-href"); ok {
		return v
	}
	return ""
}

// processVoicemail resolves an audio URL if needed, downloads the mp3 (or
// skips the download if the file already exists on disk), and fires an
// Apprise notification. Returns the audio URL actually used, so the caller
// can persist it in the seen-state even when it had to be resolved via
// get_link.
func processVoicemail(client *http.Client, cfg *Config, base, mp3Dir, id, caller, timestamp, audioURL string) (string, error) {
	if audioURL == "" {
		csrf := fetchCSRFToken(client, base+"/voicemail")
		link, err := getDownloadLink(client, base+"/voicemail", id, "INBOX", csrf)
		if err != nil {
			return "", fmt.Errorf("no audio url found in row and get_link failed: %w", err)
		}
		audioURL = link
		slog.Info("resolved audio url via get_link", "id", id, "url", audioURL)
	}

	if strings.HasPrefix(audioURL, "/") {
		audioURL = base + audioURL
	}

	filename := filepath.Join(mp3Dir, id+".mp3")
	if fi, statErr := os.Stat(filename); statErr == nil && !fi.IsDir() {
		slog.Info("file already exists, marking as seen", "file", filename, "id", id)
		return audioURL, nil
	}

	if err := downloadMP3(client, audioURL, filename); err != nil {
		return audioURL, fmt.Errorf("download failed: %w", err)
	}
	slog.Info("saved voicemail", "file", filename)

	if len(cfg.AppriseURLs) > 0 {
		subj := fmt.Sprintf("New voicemail from %s", caller)
		body := fmt.Sprintf("Voicemail received at %s from %s", timestamp, caller)
		if err := notifyApprise(cfg, subj, body, filename); err != nil {
			slog.Error("failed sending apprise notification", "err", err)
		} else {
			slog.Info("sent apprise notification")
		}
	}

	return audioURL, nil
}

func downloadMP3(client *http.Client, urlStr, filename string) error {
	resp, err := client.Get(urlStr)
	if err != nil {
		return fmt.Errorf("error downloading: %w", err)
	}
	defer resp.Body.Close()

	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("error creating file: %w", err)
	}
	defer file.Close()

	if _, err := io.Copy(file, resp.Body); err != nil {
		return fmt.Errorf("error writing file: %w", err)
	}
	return nil
}

// notifyApprise sends a notification with the mp3 attached via
// github.com/unraid/apprise-go, a pure-Go reimplementation of
// https://github.com/caronc/apprise — no separate process or container
// involved. Apprise itself fans a single Send out to whatever services
// cfg.AppriseURLs names (email, Discord, ntfy, Telegram, Pushover, ...);
// this binary doesn't need to know which.
func notifyApprise(cfg *Config, title, body, attachmentPath string) error {
	client := apprise.New()
	for _, u := range cfg.AppriseURLs {
		if err := client.Add(u); err != nil {
			return fmt.Errorf("invalid apprise url: %w", err)
		}
	}
	return client.Send(body, apprise.WithTitle(title), apprise.WithAttachments(attachmentPath))
}

// fetchCSRFToken tries to extract a CSRF token from the given page.
// It checks for a meta[name="csrf-token"] or input[name="authenticity_token"].
func fetchCSRFToken(client *http.Client, pageURL string) string {
	resp, err := client.Get(pageURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return ""
	}
	if v, ok := doc.Find("meta[name='csrf-token']").Attr("content"); ok {
		return v
	}
	if v, ok := doc.Find("input[name='authenticity_token']").Attr("value"); ok {
		return v
	}
	// fallback: try cookie named token
	for _, c := range client.Jar.Cookies(resp.Request.URL) {
		if c.Name == "token" || c.Name == "csrf-token" {
			return c.Value
		}
	}
	return ""
}

// getDownloadLink calls the /phone/voicemail/get_link endpoint with the message id
// and returns the URL from the JSON response.
func getDownloadLink(client *http.Client, basePageURL, id, folder, csrfToken string) (string, error) {
	u, err := url.Parse(basePageURL)
	if err != nil {
		return "", fmt.Errorf("invalid base url: %w", err)
	}
	endpoint := u.Scheme + "://" + u.Host + "/phone/voicemail/get_link"
	vals := url.Values{}
	vals.Set("message[id]", id)
	vals.Set("message[folder]", folder)
	reqURL := endpoint + "?" + vals.Encode()

	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("Referer", u.Scheme+"://"+u.Host+"/voicemail")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36")
	if csrfToken != "" {
		req.Header.Set("X-CSRF-Token", csrfToken)
	}
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("get_link returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Success bool   `json:"success"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("failed parsing get_link json: %w; body=%s", err, string(body))
	}
	if !parsed.Success || parsed.URL == "" {
		return "", fmt.Errorf("get_link did not return url: %s", string(body))
	}
	return parsed.URL, nil
}

func loadSeen(path string) map[string]Voicemail {
	seen := make(map[string]Voicemail)
	file, err := os.Open(path)
	if err != nil {
		return seen
	}
	defer file.Close()
	_ = json.NewDecoder(file).Decode(&seen)
	return seen
}

func saveSeen(path string, seen map[string]Voicemail) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("error saving state: %w", err)
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(seen); err != nil {
		return fmt.Errorf("error encoding state: %w", err)
	}
	return nil
}

// loginOoma performs a best-effort POST to the provided loginURL with
// username/password. This entire function is reverse-engineered against
// my.ooma.com's login page as observed in December 2025 — Ooma has no
// documented login API, so it tries a handful of plausible field-name/path
// combinations rather than relying on exactly one. If this stops working,
// Ooma likely changed their login form; inspect the current page's <form>
// (it's dumped by the final fallback below) and adjust field names here.
func loginOoma(client *http.Client, loginURL, username, password string) error {
	// Helper to read a short snippet of the response body for diagnostics
	readSnippet := func(r io.Reader, max int64) string {
		b, _ := io.ReadAll(io.LimitReader(r, max))
		return strings.TrimSpace(string(b))
	}

	// Ensure we have any initial cookies and try to extract an authenticity_token by doing a GET first
	var extractedToken string
	if respGet, err := client.Get(loginURL); err == nil {
		// attempt to parse form for authenticity_token
		doc, err := goquery.NewDocumentFromReader(io.LimitReader(respGet.Body, 1<<20))
		if err == nil {
			if v, ok := doc.Find("input[name='authenticity_token']").Attr("value"); ok {
				extractedToken = v
			}
		}
		io.Copy(io.Discard, io.LimitReader(respGet.Body, 1024))
		respGet.Body.Close()
	}

	// Try a set of likely form field names and endpoints
	tries := []struct {
		url  string
		vals url.Values
	}{}

	// primary: username/password to provided URL
	vals := url.Values{}
	vals.Set("username", username)
	vals.Set("password", password)
	vals.Set("remember_me", "on")
	vals.Set("button", "")
	if extractedToken != "" {
		vals.Set("authenticity_token", extractedToken)
	}
	tries = append(tries, struct {
		url  string
		vals url.Values
	}{loginURL, vals})

	// fallback: email field
	vals2 := url.Values{}
	vals2.Set("email", username)
	vals2.Set("password", password)
	vals2.Set("remember_me", "on")
	vals2.Set("button", "")
	if extractedToken != "" {
		vals2.Set("authenticity_token", extractedToken)
	}
	tries = append(tries, struct {
		url  string
		vals url.Values
	}{loginURL, vals2})

	// try common login paths
	commonPaths := []string{"/login", "/signin", "/session"}
	for _, p := range commonPaths {
		tries = append(tries, struct {
			url  string
			vals url.Values
		}{loginURL + p, vals})
		tries = append(tries, struct {
			url  string
			vals url.Values
		}{loginURL + p, vals2})
	}

	var lastErr error
	// Helper to POST form values with headers similar to the curl example
	doPostFormWithHeaders := func(urlStr string, vals url.Values, referer string) (*http.Response, error) {
		req, err := http.NewRequest("POST", urlStr, strings.NewReader(vals.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		// set Origin to login host
		if strings.HasPrefix(loginURL, "http") {
			// derive origin (scheme+host)
			if u, err := url.Parse(loginURL); err == nil {
				req.Header.Set("Origin", u.Scheme+"://"+u.Host)
			}
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36")
		return client.Do(req)
	}

	for _, t := range tries {
		resp, err := doPostFormWithHeaders(t.url, t.vals, loginURL)
		if err != nil {
			lastErr = err
			continue
		}
		bodySnippet := readSnippet(resp.Body, 8192)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			return nil
		}
		lastErr = fmt.Errorf("login to %s returned status %d: %s", t.url, resp.StatusCode, bodySnippet)
	}

	// As a more advanced fallback: try parsing a login form from the login page
	if resp, err := client.Get(loginURL); err == nil {
		defer resp.Body.Close()
		doc, err := goquery.NewDocumentFromReader(resp.Body)
		if err == nil {
			// find the form that contains a password input
			found := false
			doc.Find("form").EachWithBreak(func(i int, s *goquery.Selection) bool {
				if s.Find("input[type='password']").Length() == 0 {
					return true // continue
				}
				found = true
				action, _ := s.Attr("action")
				if action == "" {
					action = loginURL
				}
				actionURL := action
				if !strings.HasPrefix(action, "http") {
					// resolve relative
					if strings.HasSuffix(loginURL, "/") {
						actionURL = loginURL + strings.TrimPrefix(action, "/")
					} else {
						actionURL = loginURL + action
					}
				}

				formVals := url.Values{}
				// collect inputs
				s.Find("input").Each(func(i int, in *goquery.Selection) {
					name, _ := in.Attr("name")
					if name == "" {
						return
					}
					inpType := strings.ToLower(in.AttrOr("type", ""))
					if inpType == "password" {
						formVals.Set(name, password)
						return
					}
					if strings.Contains(strings.ToLower(name), "user") || strings.Contains(strings.ToLower(name), "email") || strings.Contains(strings.ToLower(name), "login") {
						formVals.Set(name, username)
						return
					}
					// hidden / other fields: keep existing value if present
					val, _ := in.Attr("value")
					if val != "" {
						formVals.Set(name, val)
					}
				})

				// POST the assembled form
				resp2, err := client.PostForm(actionURL, formVals)
				if err != nil {
					lastErr = err
					return false
				}
				snippet := readSnippet(resp2.Body, 8192)
				resp2.Body.Close()
				if resp2.StatusCode >= 200 && resp2.StatusCode < 400 {
					lastErr = nil
				} else {
					lastErr = fmt.Errorf("form login to %s returned %d: %s", actionURL, resp2.StatusCode, snippet)
				}
				return false // stop iterating forms
			})
			if found && lastErr == nil {
				return nil
			}
		}
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("login failed: unknown error")
}

package web

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tagger"
	"golang.org/x/crypto/bcrypt"
)

type executionProviderRow struct {
	Name  string
	Label string
}

// Not probed here: the parent must not load ONNX Runtime on a page
// render, so a bad pick is refused at save time.
func executionProviderRows() []executionProviderRow {
	rows := make([]executionProviderRow, 0, len(config.ValidExecutionProviders))
	for _, name := range config.ValidExecutionProviders {
		rows = append(rows, executionProviderRow{Name: name, Label: providerDisplayLabel(name)})
	}
	return rows
}

var providerDisplayLabels = map[string]string{
	"cpu":      "CPU",
	"cuda":     "CUDA",
	"directml": "DirectML",
	"tensorrt": "TensorRT",
	"openvino": "OpenVINO",
	"coreml":   "CoreML",
	"coremlv2": "CoreML V2",
}

func providerDisplayLabel(name string) string { return cmp.Or(providerDisplayLabels[name], name) }

// Galleries shadows the layout's list with the rows this page's table renders.
type settingsData struct {
	baseData
	Galleries          []galleryRow
	Config             *config.Config
	Taggers            []tagger.TaggerStatus
	TaggerRows         []taggerRow
	ScheduleStatus     ScheduleStatus
	ScheduleScope      scheduleScope
	Stats              statsData
	ExecutionProviders []executionProviderRow
	ScheduleModes      []scheduleModeRow
	PluginPending      []pairReq
	PluginPaired       int
	PluginRows         []pluginRowView
	PluginsDir         string
	Themes             themeCluster
	ThemesDir          string
	DesktopProfile     bool
	DesktopFolders     []string
	Desktop            desktopIntegration
	DesktopJobWarning  string
}

func (s *Server) settingsHandler(w http.ResponseWriter, r *http.Request) {
	base := s.base(r, "settings", "Settings - "+s.booruName())
	s.disableUnavailableTaggers()
	s.persistNewlyDiscoveredTaggers()
	taggers := tagger.AvailableTaggers(s.cfgSnapshot())
	modelPath := s.modelPath()
	catalog := tagger.LoadCatalog(modelPath)
	taggerByName := map[string]tagger.TaggerStatus{}
	for _, t := range taggers {
		taggerByName[t.Name] = t
	}
	var supportedRows, unsupportedRows []taggerRow
	totalGalleries := len(s.galleries())
	catalogNames := map[string]bool{}
	for _, e := range catalog {
		catalogNames[e.Name] = true
		if t, installed := taggerByName[e.Name]; installed {
			row := s.installedTaggerRow(t, totalGalleries, modelPath)
			row.Supported = true
			row.Description = e.Description
			row.Gated = e.Gated
			row.HostCommand = e.HostCommand()
			row.DockerCommand = e.DockerCommand("monbooru")
			supportedRows = append(supportedRows, row)
		} else {
			supportedRows = append(supportedRows, taggerRow{
				Name:          e.Name,
				Description:   e.Description,
				Supported:     true,
				Gated:         e.Gated,
				HostCommand:   e.HostCommand(),
				DockerCommand: e.DockerCommand("monbooru"),
				Files:         e.Files,
				TargetDir:     filepath.Join(modelPath, e.Name),
			})
		}
	}
	for _, t := range taggers {
		if catalogNames[t.Name] {
			continue
		}
		unsupportedRows = append(unsupportedRows, s.installedTaggerRow(t, totalGalleries, modelPath))
	}
	taggerRows := append(supportedRows, unsupportedRows...)
	s.renderTemplate(w, "settings.html", settingsData{
		baseData:           base,
		Galleries:          s.galleryRowsWithSnapshot(s.activeGallery(), base.VisibleCount, base.TagCount),
		Config:             s.cfgSnapshot(),
		Taggers:            taggers,
		TaggerRows:         taggerRows,
		ScheduleStatus:     s.scheduleStatus(),
		ScheduleScope:      scheduleScopeView(s.cfgSnapshot(), base.MonloaderPaired),
		Stats:              s.gatherStats(),
		ExecutionProviders: executionProviderRows(),
		ScheduleModes:      scheduleModeRows(),
		PluginPending:      s.pairs.listPending(),
		PluginPaired:       s.pairedPeerCount(),
		PluginRows:         s.pluginRows(),
		PluginsDir:         s.pluginsDir(),
		Themes:             s.themeCluster(),
		ThemesDir:          s.themesDir(),
		DesktopProfile:     s.desktopLocal(r),
		DesktopFolders:     s.availableFolders(),
		Desktop:            s.desktopIntegration(),
		DesktopJobWarning:  s.desktopJobWarning(),
	})
}

func (s *Server) settingsSchedulePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	timeVal := strings.TrimSpace(r.FormValue("time"))
	timeVal = cmp.Or(timeVal, "01:00")
	if err := config.ValidateScheduleTime(timeVal); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	mode := strings.TrimSpace(r.FormValue("mode"))
	if !config.IsValidScheduleMode(mode) {
		mode = config.ScheduleAtTime
	}
	s.cfgMu.Lock()
	s.cfg.Schedule.Time = timeVal
	s.cfg.Schedule.Mode = mode
	s.cfg.Schedule.SyncGallery = r.FormValue("sync_gallery") == "on"
	s.cfg.Schedule.RemoveOrphans = r.FormValue("remove_orphans") == "on"
	s.cfg.Schedule.RunAutoTaggers = r.FormValue("run_auto_taggers") == "on"
	s.cfg.Schedule.FindRelationPairs = r.FormValue("find_relation_pairs") == "on"
	// Drawn only while monloader is paired; a box never on the page was
	// not unticked.
	if r.FormValue("lookup_switches") != "" {
		s.cfg.Schedule.LookupPTR = r.FormValue("lookup_ptr") == "on"
		s.cfg.Schedule.LookupBooru = r.FormValue("lookup_booru") == "on"
	}
	s.cfg.Schedule.Galleries = scheduleGalleriesForm(r, s.cfg.Schedule.Galleries, s.cfg.Galleries)
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	s.sched.requestReload()
	logx.Infof("settings: schedule updated (time=%s mode=%s)", timeVal, mode)
	writeInlineFlash(w, "ok", "Saved.")
	s.renderTemplate(w, "partials/schedule_status.html", map[string]any{
		"Status": s.scheduleStatus(),
		"OOB":    true,
	})
}

// Only the columns the table drew are read: a box never on the page was
// not unticked.
func scheduleGalleriesForm(r *http.Request, lists map[string][]string, galleries []config.Gallery) map[string][]string {
	out := maps.Clone(lists)
	names := galleryNames(galleries)
	for _, action := range r.Form["scope_action"] {
		if !slices.Contains(config.ScheduleActions, action) {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		if r.FormValue("every_"+action) == "on" {
			delete(out, action)
			continue
		}
		picked := []string{}
		for _, name := range names {
			if slices.Contains(r.Form["on_"+action], name) {
				picked = append(picked, name)
			}
		}
		out[action] = picked
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type scheduleScope struct {
	Show      bool
	Galleries []string
	Columns   []scheduleColumn
}

type scheduleColumn struct {
	Action string
	Label  string
	Title  string
	Accent string
	On     bool
	Every  bool // no list: every gallery, including ones added later
	Picked map[string]bool
	Idle   map[string]bool
}

var scheduleColumns = []scheduleColumn{
	{Action: config.ActionSyncGallery, Label: "Sync", Title: "Sync gallery"},
	{Action: config.ActionRemoveOrphans, Label: "Orphans", Title: "Remove orphaned thumbnails"},
	{Action: config.ActionRunAutoTaggers, Label: "Auto-tag", Title: "Run enabled auto-taggers", Accent: "tagger-accent"},
	{Action: config.ActionFindRelationPairs, Label: "Pairs", Title: "Find relation pairs"},
	{Action: config.ActionLookupPTR, Label: "PTR", Title: "Look up unsourced images in the Public Tag Repository", Accent: "monloader-accent"},
	{Action: config.ActionLookupBooru, Label: "Boorus", Title: "Look up unsourced images on online boorus", Accent: "monloader-accent"},
}

// Shown with one gallery too while a list is set, or that list would
// restrict it unseen.
func scheduleScopeView(cfg *config.Config, monloaderPaired bool) scheduleScope {
	sched := cfg.Schedule
	names := galleryNames(cfg.Galleries)
	v := scheduleScope{Show: len(names) > 1 || len(sched.Galleries) > 0, Galleries: names}
	if !v.Show {
		return v
	}
	taggers := tagger.EnabledTaggers(cfg)
	for _, c := range scheduleColumns {
		if !monloaderPaired && (c.Action == config.ActionLookupPTR || c.Action == config.ActionLookupBooru) {
			continue
		}
		list, listed := sched.Galleries[c.Action]
		c.On, c.Every = sched.Enabled(c.Action), !listed
		c.Picked, c.Idle = map[string]bool{}, map[string]bool{}
		for _, name := range names {
			c.Picked[name] = slices.Contains(list, name)
			if c.Action == config.ActionRunAutoTaggers {
				c.Idle[name] = !slices.ContainsFunc(taggers, func(t tagger.TaggerStatus) bool { return t.AppliesToGallery(name) })
			}
		}
		v.Columns = append(v.Columns, c)
	}
	return v
}

func galleryNames(galleries []config.Gallery) []string {
	names := make([]string, len(galleries))
	for i, g := range galleries {
		names[i] = g.Name
	}
	slices.Sort(names)
	return names
}

// Not gated on the desktop profile: a container operator wants to test
// the schedule too.
func (s *Server) settingsScheduleRunPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if s.jobs.IsRunning() {
		writeInlineFlash(w, "err", "A job is already running.")
		return
	}
	go s.runScheduledActions()
	writeInlineFlash(w, "ok", "Started. Watch the job status bar.")
}

type scheduleModeRow struct {
	Name  string
	Label string
}

func scheduleModeRows() []scheduleModeRow {
	return []scheduleModeRow{
		{config.ScheduleAtTime, "Every day at the time above"},
		{config.ScheduleAtTimeCatchup, "Every day at the time above, and later if that run was missed"},
		{config.ScheduleOnStart, "At startup only, at most once a day"},
		{config.ScheduleOff, "Never"},
	}
}

func (s *Server) settingsGeneralPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	uploadFolder := strings.TrimSpace(r.FormValue("default_upload_folder"))
	folderTmpl, err := gallery.ParseNameTemplate(uploadFolder, gallery.ScopeUploadFolder)
	if err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	// A tokened folder resolves per image, so only a literal one can be
	// checked here.
	if !folderTmpl.HasTokens() {
		if _, err := s.boundary().ResolveSubdir(uploadFolder); err != nil {
			writeInlineFlash(w, "err", err.Error())
			return
		}
	}
	uploadName := strings.TrimSpace(r.FormValue("default_upload_name"))
	if _, err := gallery.ParseNameTemplate(uploadName, gallery.ScopeUploadName); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	s.cfgMu.Lock()
	s.cfg.Gallery.WatchEnabled = r.FormValue("watch_enabled") == "on"
	if n, err := strconv.Atoi(r.FormValue("max_file_size_mb")); err == nil && n >= 0 {
		s.cfg.Gallery.MaxFileSizeMB = n
	}
	s.cfg.Gallery.DefaultUploadFolder = uploadFolder
	s.cfg.Gallery.DefaultUploadName = uploadName
	s.cfg.Gallery.RenameOnIngest = r.FormValue("rename_on_ingest") == "on"
	s.cfg.Gallery.AutoMetaTags = r.FormValue("auto_meta_tags") == "on"
	if n, err := strconv.Atoi(r.FormValue("page_size")); err == nil && n > 0 {
		s.cfg.UI.PageSize = min(n, config.MaxPageSize)
	}
	if fit := r.FormValue("thumbnail_fit"); fit == "square" || fit == "natural" {
		s.cfg.UI.ThumbnailFit = fit
	}
	autoMeta := s.cfg.Gallery.AutoMetaTags
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	gallery.MetaTagsEnabled.Store(autoMeta)
	logx.Infof("settings: general updated")
	writeInlineFlash(w, "ok", "Saved.")
}

func (s *Server) settingsIgnorePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	ignore, err := config.NormalizeIgnore(strings.Split(r.FormValue("ignore"), "\n"))
	if err != nil {
		writeInlineFlash(w, "err", err.Error()+".")
		return
	}
	uploadFolder := s.cfgSnapshot().Gallery.DefaultUploadFolder
	if tmpl, err := gallery.ParseNameTemplate(uploadFolder, gallery.ScopeUploadFolder); err == nil && !tmpl.HasTokens() {
		if _, err := s.drawBoundaries(ignore)[s.activeGallery()].ResolveSubdir(uploadFolder); err != nil {
			writeInlineFlash(w, "err", "Received files go to a folder this list leaves out: "+err.Error()+".")
			return
		}
	}
	s.cfgMu.Lock()
	changed := !slices.Equal(s.cfg.Gallery.Ignore, ignore)
	s.cfg.Gallery.Ignore = ignore
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	msg := "Saved."
	if changed {
		s.rebuildBoundaries()
		msg = "Saved. A sync applies the ignore list to what is already indexed."
	}
	logx.Infof("settings: ignore list updated")
	writeInlineFlash(w, "ok", msg)
	s.renderTemplate(w, "partials/ignore_list.html", map[string]any{"Ignore": ignore, "OOB": true})
}

func (s *Server) settingsMonloaderPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.cfgMu.Lock()
	s.cfg.Server.MonloaderURL = strings.TrimSpace(r.FormValue("monloader_url"))
	s.cfg.Monloader.APIURL = strings.TrimSpace(r.FormValue("api_url"))
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: monloader link updated")
	writeInlineFlash(w, "ok", "Saved.")
}

func (s *Server) settingsPasswordPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	currentPass := r.FormValue("current_password")
	newPass := r.FormValue("new_password")
	if newPass == "" {
		writeInlineFlash(w, "err", "New password required.")
		return
	}
	if current := s.passwordHash(); current != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(current), []byte(currentPass)); err != nil {
			writeInlineFlash(w, "err", "Current password is incorrect.")
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		writeInlineFlash(w, "err", "Error hashing password.")
		return
	}
	s.cfgMu.Lock()
	s.cfg.Auth.PasswordHash = string(hash)
	s.cfg.Auth.EnablePassword = true
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: password updated from %s", clientIP(r))
	s.sessions.ClearExcept(sessionFromContext(r.Context()))
	writeInlineFlash(w, "ok", "Password updated.")
	s.renderAuthPasswordOOB(w, r)
}

func (s *Server) settingsTokenCreate(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if err := config.ValidateTokenName(name); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	tok, secret := config.GenerateToken(name, config.AllScopes)
	if err := s.withConfig(func(c *config.Config) error {
		if c.TokenNameExists(name) {
			return fmt.Errorf("a token named %q already exists", name)
		}
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	logx.Infof("settings: API token %q created from %s", name, clientIP(r))
	w.Header().Set("Cache-Control", "no-store")
	s.renderTemplate(w, "partials/flash_token.html", map[string]any{"Token": secret})
	s.renderAuthTokensOOB(w, r)
	_, _ = w.Write([]byte(`<script>(function(){var i=document.getElementById('token-name-input');if(i)i.value='';})();</script>`))
}

func (s *Server) tokenPaired(id string) bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	for _, t := range s.cfg.Auth.Tokens {
		if t.ID == id {
			return t.Paired != ""
		}
	}
	return false
}

func (s *Server) settingsTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.tokenPaired(id) {
		writeInlineFlash(w, "err", "This token is managed by a pairing; remove the pairing instead.")
		return
	}
	var removed bool
	if err := s.withConfig(func(c *config.Config) error {
		removed = c.RemoveToken(id)
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	if !removed {
		writeInlineFlash(w, "err", "Token not found.")
		return
	}
	logx.Infof("settings: API token %s revoked from %s", id, clientIP(r))
	writeInlineFlash(w, "ok", "Token revoked.")
	s.renderAuthTokensOOB(w, r)
}

func (s *Server) renderAuthTokensOOB(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	tokens := slices.Clone(s.cfg.Auth.Tokens)
	s.cfgMu.Unlock()
	s.renderTemplate(w, "partials/auth_tokens.html", map[string]any{
		"Tokens":    tokens,
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
		"OOB":       true,
	})
}

type tokenScopeRow struct {
	Name    string
	Desc    string
	Checked bool
}

func (s *Server) settingsTokenPrivilegesGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.cfgMu.Lock()
	var scopes []string
	var found, paired bool
	for _, t := range s.cfg.Auth.Tokens {
		if t.ID == id {
			scopes = slices.Clone(t.Scopes)
			paired = t.Paired != ""
			found = true
			break
		}
	}
	s.cfgMu.Unlock()
	if !found {
		http.Error(w, "token not found", http.StatusNotFound)
		return
	}
	descs := map[string]string{
		config.ScopeRead:   "read - all GET endpoints",
		config.ScopeWrite:  "write - create and modify",
		config.ScopeDelete: "delete - destructive actions",
	}
	rows := make([]tokenScopeRow, 0, len(config.AllScopes))
	for _, sc := range config.AllScopes {
		rows = append(rows, tokenScopeRow{Name: sc, Desc: descs[sc], Checked: slices.Contains(scopes, sc)})
	}
	s.renderTemplate(w, "partials/token_privileges_dialog.html", map[string]any{
		"ID":        id,
		"Scopes":    rows,
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
		"Paired":    paired,
	})
}

func (s *Server) settingsTokenPrivilegesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	id := r.PathValue("id")
	if s.tokenPaired(id) {
		writeInlineFlash(w, "err", "This token is managed by a pairing; its privileges can't be changed.")
		return
	}
	scopes := filterScopes(r.Form["scope"])
	var found bool
	if err := s.withConfig(func(c *config.Config) error {
		found = c.SetTokenScopes(id, scopes)
		return nil
	}); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	if !found {
		writeInlineFlash(w, "err", "Token not found.")
		return
	}
	logx.Infof("settings: API token %s privileges updated from %s", id, clientIP(r))
	setDialogSavedTrigger(w, "token-saved", "token-cfg-"+id)
	writeOOBSummaryFlash(w, "token-scopes-"+id, strings.Join(scopes, " "), "flash-auth", "Token privileges saved.")
}

func filterScopes(in []string) []string {
	var out []string
	for _, sc := range config.AllScopes {
		if slices.Contains(in, sc) {
			out = append(out, sc)
		}
	}
	return out
}

func (s *Server) settingsRemovePasswordPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	// Whenever a hash is set, even with the password turned off in the
	// TOML, so a file edit cannot open the remove path.
	currentPass := r.FormValue("current_password")
	if current := s.passwordHash(); current != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(current), []byte(currentPass)); err != nil {
			writeInlineFlash(w, "err", "Current password is incorrect.")
			return
		}
	}
	s.cfgMu.Lock()
	s.cfg.Auth.EnablePassword = false
	s.cfg.Auth.PasswordHash = ""
	s.cfgMu.Unlock()
	if err := s.saveConfig(); err != nil {
		writeInlineFlash(w, "err", "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: password removed from %s", clientIP(r))
	// Old sessions would otherwise stay valid when a password is set again.
	s.sessions.Clear()
	writeInlineFlash(w, "ok", "Password removed. Authentication is now disabled.")
	s.renderAuthPasswordOOB(w, r)
}

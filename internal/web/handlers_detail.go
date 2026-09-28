package web

import (
	"cmp"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/markup"
	meta "github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tagger"
)

// The render waits on Back's page; past this it keeps the page in the URL.
var rankPageBudget = 150 * time.Millisecond

type annotationView struct {
	ID    int64
	Body  template.HTML
	Style template.CSS
	doc   markup.Doc
}

type annotationEntry struct {
	models.Annotation
	Text string
}

type sourcePanelView struct {
	models.ImageSource
	Annotations       []annotationEntry
	OriginalLines     []originalLine
	CommentaryHTML    template.HTML
	TranslatedHTML    template.HTML
	HasTranslationRow bool
	doc               markup.Doc
	transDoc          markup.Doc
}

type originalLine struct {
	Text  string
	IsURL bool
}

func buildOriginalLines(original string) []originalLine {
	var out []originalLine
	for _, line := range strings.Split(original, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, originalLine{Text: line, IsURL: gallery.ValidExternalURL(line)})
	}
	return out
}

func buildAnnotationViews(img *models.Image, anns []models.Annotation, refs markup.Refs) []annotationView {
	if img.Width == nil || img.Height == nil || *img.Width <= 0 || *img.Height <= 0 || len(anns) == 0 {
		return nil
	}
	fw, fh := float64(*img.Width), float64(*img.Height)
	out := make([]annotationView, 0, len(anns))
	for _, a := range anns {
		x := min(max(a.X, 0), *img.Width)
		y := min(max(a.Y, 0), *img.Height)
		w := min(max(a.W, 0), *img.Width-x)
		h := min(max(a.H, 0), *img.Height-y)
		style := fmt.Sprintf("left:%.4f%%;top:%.4f%%;width:%.4f%%;height:%.4f%%",
			float64(x)/fw*100, float64(y)/fh*100, float64(w)/fw*100, float64(h)/fh*100)
		doc := markup.Parse(a.Body)
		doc.Collect(refs)
		out = append(out, annotationView{ID: a.ID, Style: template.CSS(style), doc: doc})
	}
	return out
}

func buildAnnotationEntries(anns []models.Annotation) []annotationEntry {
	if len(anns) == 0 {
		return nil
	}
	out := make([]annotationEntry, 0, len(anns))
	for _, a := range anns {
		out = append(out, annotationEntry{Annotation: a, Text: markup.Parse(a.Body).Text()})
	}
	return out
}

type detailData struct {
	baseData
	Image             models.Image
	Filename          string
	ImageTags         []models.ImageTag
	SDMeta            *models.SDMetadata
	ComfyMeta         *models.ComfyUIMetadata
	ComfyNodes        []models.ComfyNode
	GenericMeta       []models.SDParam
	MangaMeta         *models.MangaMetadata
	IsManga           bool
	MisnamedExt       string
	ResumePage        int
	MangaHint         string
	Collections       []models.Collection
	Sources           []models.ImageSource
	Annotations       []annotationView
	SourcePanels      []sourcePanelView
	ManualAnnotations []annotationEntry
	NoteHTML          template.HTML
	ExtraPaths        int
	ThumbnailURL      string
	PrevID            *int64
	NextID            *int64
	RefURL            string
	Ref               string
	BackQuery         string
	BackSort          string
	BackOrder         string
	BackPage          string
	BackSeed          string
	PrevBackPage      string
	PrevBackIdx       string
	NextBackPage      string
	NextBackIdx       string
	// template.URL, or html/template would escape the & separators.
	BackQS         template.URL
	BackKVQS       template.URL
	EnabledTaggers []tagger.TaggerStatus
	TaggersPresent bool
	TaggerReason   string
	ImageTaggers   []string
	ImageSources   []string
	HasUserTags    bool
	HasStaleTags   bool
	TagSidebar     tagSidebar
	Lookup         lookupView
	PhashDistance  int
	NoPreview      bool
	PreviewNote    string
	PreviewScaled  bool
	PreviewMaxDim  int
	PluginSlot     pluginSlotView
}

// External links go by sha because an image id can be reused after a delete.
func (s *Server) imageByHashHandler(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if d := s.db(); d != nil {
		var id int64
		if err := d.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, sha).Scan(&id); err == nil {
			http.Redirect(w, r, "/images/"+strconv.FormatInt(id, 10), http.StatusFound)
			return
		}
	}

	for _, cx := range s.allContexts() {
		if cx.DB == nil {
			continue
		}
		var id int64
		if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, sha).Scan(&id); err != nil {
			continue
		}
		if err := s.switchGallery(cx.Name); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, "/images/"+strconv.FormatInt(id, 10), http.StatusFound)
		return
	}
	s.notFoundHandler(w, r)
}

func (s *Server) detailHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}

	ctx := r.Context()
	img, err := loadImage(ctx, s.db(), id)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}

	back := parseBackContext(r)
	backQ, backSort, backOrder, backPage, backSeed := back.Q, back.Sort, back.Order, back.Page, back.Seed
	backIdx := r.URL.Query().Get("back_idx")

	// After a Similar-images click, back_q may not match: no prev/next.
	refURL := ""
	refStrValid := ""
	if refStr := r.URL.Query().Get("ref"); refStr != "" {
		if refID, err := strconv.ParseInt(refStr, 10, 64); err == nil && refID != id {
			refURL = back.DetailURL(refID)
			refStrValid = strconv.FormatInt(refID, 10)
		}
	}

	wantAdjacent := refURL == "" && (backSort != "" || backQ != "")
	if wantAdjacent {
		backSort = cmp.Or(backSort, "newest")
		backOrder = cmp.Or(backOrder, search.DefaultOrder(backSort))
	}
	ceiling := resolveCeiling(r, s.active())

	// Back must land on the page holding this image, wherever prev/next walked.
	// back_idx answers it free; each fallback is a query the render waits on.
	var rankPage string
	rankReady := make(chan struct{})
	rankFired := false
	pendingKey := ""
	pageSize := s.pageSize(r)
	posPage, posIdx, havePos := 0, 0, false
	if p, err := strconv.Atoi(backPage); err == nil && p > 0 && pageSize > 0 {
		if i, err := strconv.Atoi(backIdx); err == nil && i >= 0 && i < pageSize {
			posPage, posIdx, havePos = p, i, true
		}
	}
	if havePos {
		backPage = strconv.Itoa(posPage)
	}
	if wantAdjacent && pageSize > 0 && !havePos {
		var seed int64
		if backSort == "random" && backSeed != "" {
			seed, _ = strconv.ParseInt(backSeed, 10, 64)
		}
		cacheKey := search.BuildAdjacencyCacheKey(s.activeGallery(), backQ, backSort, backOrder, seed, ceiling.Level())
		switch ids, ok := search.AdjacencyCacheGet(cacheKey); {
		case ok:
			// A miss here means back_q no longer matches; don't rank it.
			if page, found := s.pageOfMatch(ids, id, pageSize); found {
				backPage = page
			}
		case backSort == "similarity":
			// A rank by score would re-run the fan the adjacency lookup pays;
			// the page comes off that lookup's cached list afterwards.
			pendingKey = cacheKey
		default:
			rankFired = true
			go func() {
				defer close(rankReady)
				sq := adjacentSearchQuery(backQ, backSort, backOrder, backSeed, ceiling)
				ctx, cancel := context.WithTimeout(r.Context(), rankPageBudget)
				defer cancel()
				if arrived, err := strconv.Atoi(backPage); err == nil {
					if page, ok := s.pageHoldingMatch(ctx, sq, id, arrived, pageSize); ok {
						rankPage = strconv.Itoa(page)
						return
					}
				}
				rank, err := search.RankInQuery(ctx, s.db(), sq, id)
				if err != nil || rank < 0 {
					return
				}
				rankPage = strconv.Itoa(rank/pageSize + 1)
			}()
		}
	}
	if !rankFired {
		close(rankReady)
	}

	// Related images load lazily; their tag join would dominate this render.
	var (
		imageTags   []models.ImageTag
		sdMeta      *models.SDMetadata
		comfyMeta   *models.ComfyUIMetadata
		genericMeta []models.SDParam
		mangaMeta   *models.MangaMetadata
		imagePaths  []models.ImagePath
		collections []models.Collection
		sources     []models.ImageSource
		annotations []models.Annotation
		sidebar     tagSidebar
		lookupState lookupView
		prevID      *int64
		nextID      *int64
	)
	csrfToken := s.csrfToken(sessionFromContext(ctx))
	tagMode := readTagModeCookie(r)
	isManga := img.FileType == models.FileTypeCBZ
	misnamedExt := ""
	if claimed := gallery.ExtFileType(img.CanonicalPath); claimed != "" && claimed != img.FileType {
		misnamedExt = strings.ToLower(filepath.Ext(img.CanonicalPath))
	}
	var wg sync.WaitGroup
	wg.Add(9)
	go func() {
		defer wg.Done()
		_, imageTags, _ = s.tagSvc().GetImageTags(id)
		sidebar = s.buildTagSidebar(id, csrfToken, tagMode, imageTags)
	}()
	go func() { defer wg.Done(); sdMeta = loadSDMeta(ctx, s.db(), id) }()
	go func() { defer wg.Done(); comfyMeta = loadComfyMeta(ctx, s.db(), id) }()
	go func() { defer wg.Done(); imagePaths = loadImagePaths(ctx, s.db(), s.boundary(), id) }()
	go func() { defer wg.Done(); collections, _ = gallery.CollectionsForImage(s.db(), id) }()
	go func() { defer wg.Done(); sources, _ = gallery.SourcesForImage(s.db(), id) }()
	go func() { defer wg.Done(); lookupState = s.lookupViewFor(s.active(), id) }()
	go func() { defer wg.Done(); annotations, _ = gallery.AnnotationsForImage(s.db(), id) }()
	go func() {
		defer wg.Done()
		if !isManga {
			genericMeta = meta.ExtractGeneric(img.CanonicalPath, img.FileType)
		}
	}()
	if isManga {
		wg.Add(1)
		go func() { defer wg.Done(); mangaMeta = loadMangaMeta(ctx, s.db(), id) }()
	}
	if wantAdjacent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prevID, nextID = s.findAdjacentImages(ctx, id, backQ, backSort, backOrder, backSeed, ceiling)
		}()
	}
	wg.Wait()
	<-rankReady
	if rankPage != "" {
		backPage = rankPage
	} else if ids, ok := search.AdjacencyCacheGet(pendingKey); ok {
		if page, found := s.pageOfMatch(ids, id, pageSize); found {
			backPage = page
		}
	}

	prevBackPage, prevBackIdx, nextBackPage, nextBackIdx := backPage, "", backPage, ""
	if havePos {
		if posIdx > 0 {
			prevBackPage, prevBackIdx = strconv.Itoa(posPage), strconv.Itoa(posIdx-1)
		} else if posPage > 1 {
			prevBackPage, prevBackIdx = strconv.Itoa(posPage-1), strconv.Itoa(pageSize-1)
		}
		if posIdx+1 < pageSize {
			nextBackPage, nextBackIdx = strconv.Itoa(posPage), strconv.Itoa(posIdx+1)
		} else {
			nextBackPage, nextBackIdx = strconv.Itoa(posPage+1), "0"
		}
	}

	var comfyNodes []models.ComfyNode
	if comfyMeta != nil && comfyMeta.RawWorkflow != "" {
		comfyNodes = meta.ParseComfyWorkflowNodes(comfyMeta.RawWorkflow)
	}

	taggerCfg := s.cfgSnapshot()
	enabledTaggers := tagger.EnabledTaggersForGallery(taggerCfg, s.activeGallery())
	imageTaggers := distinctTaggerNames(imageTags, true)
	imageSources := distinctTaggerNames(imageTags, false)
	hasUserTags, hasStaleTags := userAndStaleTags(imageTags)

	var manualAnnotations []models.Annotation
	annBySource := map[[2]string][]models.Annotation{}
	for _, a := range annotations {
		if a.Manual {
			manualAnnotations = append(manualAnnotations, a)
			continue
		}
		k := [2]string{a.Site, a.PostID}
		annBySource[k] = append(annBySource[k], a)
	}
	// All bodies Collect first; resolveMarkup looks the refs up in one batch.
	mkRefs := markup.NewRefs()
	noteDoc := markup.Parse(img.Note)
	noteDoc.Collect(mkRefs)
	annotationViews := buildAnnotationViews(img, annotations, mkRefs)
	var sourcePanels []sourcePanelView
	for _, src := range sources {
		boxes := annBySource[[2]string{src.Site, src.PostID}]
		if src.Commentary == "" && src.CommentaryTranslated == "" && src.Original == "" && len(boxes) == 0 {
			continue
		}
		doc := markup.Parse(src.Commentary)
		doc.Collect(mkRefs)
		transDoc := markup.Parse(src.CommentaryTranslated)
		transDoc.Collect(mkRefs)
		sourcePanels = append(sourcePanels, sourcePanelView{
			ImageSource:       src,
			Annotations:       buildAnnotationEntries(boxes),
			OriginalLines:     buildOriginalLines(src.Original),
			HasTranslationRow: src.Commentary != "" || src.CommentaryTranslated != "",
			doc:               doc,
			transDoc:          transDoc,
		})
	}
	mkRes := s.resolveMarkup(mkRefs)
	for i := range annotationViews {
		annotationViews[i].Body = annotationViews[i].doc.Render(mkRes)
	}
	for i := range sourcePanels {
		sourcePanels[i].CommentaryHTML = sourcePanels[i].doc.Render(mkRes)
		sourcePanels[i].TranslatedHTML = sourcePanels[i].transDoc.Render(mkRes)
	}

	noPreview, previewNote := false, ""
	if _, statErr := os.Stat(gallery.ThumbnailPath(s.thumbnailsPath(), img.ID)); statErr != nil {
		noPreview = true
		previewNote = gallery.PreviewRefusal(img.CanonicalPath, img.FileType)
	}
	var pxW, pxH int
	if img.Width != nil && img.Height != nil {
		pxW, pxH = *img.Width, *img.Height
	}
	previewScaled := !isManga && !gallery.IsVideoType(img.FileType) && gallery.NeedsViewRendition(pxW, pxH)

	baseName := filepath.Base(img.CanonicalPath)
	// FolderPath, not the disk dir: a root-level file has no folder to name.
	titleName := baseName
	if img.FolderPath != "" {
		titleName = path.Base(img.FolderPath) + "/" + baseName
	}
	mangaHint := ""
	if isManga {
		mangaHint = strings.TrimSuffix(baseName, filepath.Ext(baseName))
		if mangaMeta != nil {
			if series := strings.TrimSpace(mangaMeta.Series); series != "" {
				mangaHint = series
			} else if title := strings.TrimSpace(mangaMeta.Title); title != "" {
				mangaHint = title
			}
		}
	}
	data := detailData{
		baseData:          s.base(r, "gallery", fmt.Sprintf("%s - %s", titleName, s.booruName())),
		Image:             *img,
		Filename:          baseName,
		ImageTags:         imageTags,
		SDMeta:            sdMeta,
		ComfyMeta:         comfyMeta,
		ComfyNodes:        comfyNodes,
		GenericMeta:       genericMeta,
		MangaMeta:         mangaMeta,
		IsManga:           isManga,
		MisnamedExt:       misnamedExt,
		MangaHint:         mangaHint,
		ResumePage:        resumePage(img),
		Collections:       collections,
		Sources:           sources,
		Annotations:       annotationViews,
		SourcePanels:      sourcePanels,
		ManualAnnotations: buildAnnotationEntries(manualAnnotations),
		NoteHTML:          noteDoc.Render(mkRes),
		ExtraPaths:        extraImagePaths(imagePaths),
		ThumbnailURL:      fmt.Sprintf("/thumbnails/%s/%d.jpg", s.activeGallery(), id),
		PrevID:            prevID,
		NextID:            nextID,
		RefURL:            refURL,
		Ref:               refStrValid,
		BackQuery:         backQ,
		BackSort:          backSort,
		BackOrder:         backOrder,
		BackPage:          backPage,
		BackSeed:          backSeed,
		PrevBackPage:      prevBackPage,
		PrevBackIdx:       prevBackIdx,
		NextBackPage:      nextBackPage,
		NextBackIdx:       nextBackIdx,
		BackQS:            back.QueryString("?"),
		BackKVQS:          back.QueryString("&"),
		EnabledTaggers:    enabledTaggers,
		TaggersPresent:    tagger.Present(taggerCfg),
		TaggerReason:      tagger.UnavailableReason(taggerCfg),
		ImageTaggers:      imageTaggers,
		ImageSources:      imageSources,
		HasUserTags:       hasUserTags,
		HasStaleTags:      hasStaleTags,
		TagSidebar:        sidebar,
		Lookup:            lookupState,
		PhashDistance:     s.findPairsDistance(),
		NoPreview:         noPreview,
		PreviewNote:       previewNote,
		PreviewScaled:     previewScaled,
		PreviewMaxDim:     gallery.ViewMaxDim,
		PluginSlot:        s.pluginSlot(r, config.SlotDetailActions, id, img.FileType),
	}
	s.renderTemplate(w, "detail.html", data)
}

func (s *Server) relatedImagesHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !isHTMXRequest(r) {
		dst := "/images/" + idStr
		if r.URL.RawQuery != "" {
			dst += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, dst, http.StatusSeeOther)
		return
	}
	related, _ := s.tagSvc().RelatedImages(id, 6, readRatingCookie(r))
	back := parseBackContext(r)
	s.renderTemplate(w, "partials/related_images.html", map[string]any{
		"Images":        related,
		"ActiveGallery": s.activeGallery(),
		"SourceID":      id,
		"BackQuery":     back.Q,
		"BackSort":      back.Sort,
		"BackOrder":     back.Order,
		"BackPage":      back.Page,
		"BackSeed":      back.Seed,
	})
}

// A source row's tagger_name is its source label, so auto=false lists sources.
func distinctTaggerNames(tags []models.ImageTag, auto bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		if t.IsAuto != auto || t.TaggerName == "" || seen[t.TaggerName] {
			continue
		}
		seen[t.TaggerName] = true
		out = append(out, t.TaggerName)
	}
	return out
}

func (s *Server) pageOfMatch(ids []int64, id int64, pageSize int) (string, bool) {
	i := slices.Index(ids, id)
	if i < 0 {
		return "", false
	}
	return strconv.Itoa(i/pageSize + 1), true
}

// One prev/next step reaches at most a neighbour page, and a page-sized
// read costs the same at any depth, where a rank COUNT grows with it.
func (s *Server) pageHoldingMatch(ctx context.Context, sq search.Query, id int64, arrived, pageSize int) (int, bool) {
	if arrived < 1 || pageSize < 1 {
		return 0, false
	}
	for _, page := range []int{arrived, arrived + 1, arrived - 1} {
		if page < 1 || ctx.Err() != nil {
			continue
		}
		probe := sq
		probe.Page, probe.Limit = page, pageSize
		probe.SkipCount, probe.CacheKey = true, ""
		res, err := search.Execute(s.db(), probe)
		if err != nil {
			return 0, false
		}
		if slices.ContainsFunc(res.Results, func(img models.Image) bool { return img.ID == id }) {
			return page, true
		}
	}
	return 0, false
}

func (s *Server) findAdjacentImages(ctx context.Context, currentID int64, queryStr, sortStr, orderStr, seedStr string, ceiling *Ceiling) (prevID, nextID *int64) {
	sq := adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr, ceiling)
	sq.CacheKey = search.BuildAdjacencyCacheKey(s.activeGallery(), queryStr, sortStr, orderStr, sq.RandomSeed, ceiling.Level())
	prevID, nextID, err := search.ExecuteAdjacent(ctx, s.db(), sq, currentID)
	if err != nil {
		logx.Warnf("findAdjacentImages: %v", err)
	}
	return
}

func adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr string, ceiling *Ceiling) search.Query {
	expr := search.Parse(queryStr)
	pinnedCollection := search.PinnedCollectionName(expr)
	expr = ceiling.Apply(expr)
	sq := search.Query{
		Expr:  expr,
		Sort:  sortStr,
		Order: orderStr,
	}
	if sortStr == "order" {
		sq.OrderCollection = pinnedCollection
	}
	if sortStr == "random" && seedStr != "" {
		if seed, err := strconv.ParseInt(seedStr, 10, 64); err == nil {
			sq.RandomSeed = seed
		}
	}
	return sq
}

// A file over the ingest cap is left to the backfill: too big to hash per view.
func (s *Server) md5CellGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx := s.active()
	if cx == nil || cx.DB == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if maxMB := s.maxFileSizeMB(); maxMB > 0 {
		var size int64
		if err := cx.DB.Read.QueryRow(`SELECT file_size FROM images WHERE id = ?`, id).Scan(&size); err == nil &&
			size > int64(maxMB)*1024*1024 {
			s.renderTemplate(w, "partials/md5_cell.html", map[string]any{"Deferred": true})
			return
		}
	}
	sum, err := gallery.ComputeAndStoreMD5(r.Context(), cx.DB, id)
	if err != nil {
		logx.Debugf("md5 cell %d: %v", id, err)
	}
	s.renderTemplate(w, "partials/md5_cell.html", map[string]any{"MD5": sum})
}

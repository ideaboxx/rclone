// Package drive interfaces with the Google Drive object storage system
package clouddrive

// FIXME need to deal with some corner cases
// * multiple files with the same name
// * files can be in multiple directories
// * can have directory loops
// * files with / in name

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"

	"mime"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/fspath"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/walk"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"

	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/readers"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	drive_v2 "google.golang.org/api/drive/v2"
	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// Constants
const (
	rcloneClientID              = "202264815644.apps.googleusercontent.com"
	rcloneEncryptedClientSecret = "eX8GpZTVx3vxMWVkuuBdDWmAUE6rGhTwVrvG9GhllYccSdj2-mvHVg"
	driveFolderType             = "application/vnd.google-apps.folder"
	shortcutMimeType            = "application/vnd.google-apps.shortcut"
	shortcutMimeTypeDangling    = "application/vnd.google-apps.shortcut.dangling" // synthetic mime type for internal use
	timeFormatIn                = time.RFC3339
	timeFormatOut               = "2006-01-02T15:04:05.000000000Z07:00"
	defaultMinSleep             = fs.Duration(100 * time.Millisecond)
	defaultBurst                = 100
	defaultExportExtensions     = "docx,xlsx,pptx,svg"
	scopePrefix                 = "https://www.googleapis.com/auth/"
	defaultScope                = "drive"
	// chunkSize is the size of the chunks created during a resumable upload and should be a power of two.
	// 1<<18 is the minimum size supported by the Google uploader, and there is no maximum.
	minChunkSize     = fs.SizeSuffix(googleapi.MinUploadChunkSize)
	defaultChunkSize = 8 * fs.Mebi
	partialFields    = "id,name,size,md5Checksum,trashed,description,explicitlyTrashed,modifiedTime,createdTime,mimeType,parents,webViewLink,shortcutDetails,exportLinks,resourceKey"
	listRGrouping    = 50   // number of IDs to search at once when using ListR
	listRInputBuffer = 1000 // size of input buffer when using ListR
	defaultXDGIcon   = "text-html"
)

// Globals
var (
	_mimeTypeToExtensionDuplicates = map[string]string{
		"application/x-vnd.oasis.opendocument.presentation": ".odp",
		"application/x-vnd.oasis.opendocument.spreadsheet":  ".ods",
		"application/x-vnd.oasis.opendocument.text":         ".odt",
		"image/jpg":   ".jpg",
		"image/x-bmp": ".bmp",
		"image/x-png": ".png",
		"text/rtf":    ".rtf",
	}
	_mimeTypeToExtension = map[string]string{
		"application/epub+zip":                            ".epub",
		"application/json":                                ".json",
		"application/msword":                              ".doc",
		"application/pdf":                                 ".pdf",
		"application/rtf":                                 ".rtf",
		"application/vnd.ms-excel":                        ".xls",
		"application/vnd.oasis.opendocument.presentation": ".odp",
		"application/vnd.oasis.opendocument.spreadsheet":  ".ods",
		"application/vnd.oasis.opendocument.text":         ".odt",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
		"application/x-msmetafile":  ".wmf",
		"application/zip":           ".zip",
		"image/bmp":                 ".bmp",
		"image/jpeg":                ".jpg",
		"image/pjpeg":               ".pjpeg",
		"image/png":                 ".png",
		"image/svg+xml":             ".svg",
		"text/csv":                  ".csv",
		"text/html":                 ".html",
		"text/plain":                ".txt",
		"text/tab-separated-values": ".tsv",
	}
	_mimeTypeToExtensionLinks = map[string]string{
		"application/x-link-desktop": ".desktop",
		"application/x-link-html":    ".link.html",
		"application/x-link-url":     ".url",
		"application/x-link-webloc":  ".webloc",
	}
	_mimeTypeCustomTransform = map[string]string{
		"application/vnd.google-apps.script+json": "application/json",
	}
	_mimeTypeToXDGLinkIcons = map[string]string{
		"application/vnd.google-apps.document":     "x-office-document",
		"application/vnd.google-apps.drawing":      "x-office-drawing",
		"application/vnd.google-apps.presentation": "x-office-presentation",
		"application/vnd.google-apps.spreadsheet":  "x-office-spreadsheet",
	}
	fetchFormatsOnce sync.Once                     // make sure we fetch the export/import formats only once
	_exportFormats   map[string][]string           // allowed export MIME type conversions
	_importFormats   map[string][]string           // allowed import MIME type conversions
	templatesOnce    sync.Once                     // parse link templates only once
	_linkTemplates   map[string]*template.Template // available link types
)

// Parse the scopes option returning a slice of scopes
func driveScopes(scopesString string) (scopes []string) {
	if scopesString == "" {
		scopesString = defaultScope
	}
	for _, scope := range strings.Split(scopesString, ",") {
		scope = strings.TrimSpace(scope)
		scopes = append(scopes, scopePrefix+scope)
	}
	return scopes
}

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "clouddrive",
		Description: "Cloud Drive",
		NewFs:       NewFs,
		CommandHelp: CommandHelp,
		Options:     scopes,
	})

	// register duplicate MIME types first
	// this allows them to be used with mime.ExtensionsByType() but
	// mime.TypeByExtension() will return the later registered MIME type
	for _, m := range []map[string]string{
		_mimeTypeToExtensionDuplicates, _mimeTypeToExtension, _mimeTypeToExtensionLinks,
	} {
		for mimeType, extension := range m {
			if err := mime.AddExtensionType(extension, mimeType); err != nil {
				fs.Errorf("Failed to register MIME type %q: %v", mimeType, err)
			}
		}
	}
}

// Options defines the configuration for this backend
type Options struct {
	Scope                   string               `config:"scope"`
	RootFolderID            string               `config:"root_folder_id"`
	MasterKeyFile           string               `config:"master_key_file"`
	CopyShortcutContent     bool                 `config:"copy_shortcut_content"`
	SkipGdocs               bool                 `config:"skip_gdocs"`
	SkipChecksumGphotos     bool                 `config:"skip_checksum_gphotos"`
	SharedWithMe            bool                 `config:"shared_with_me"`
	TrashedOnly             bool                 `config:"trashed_only"`
	StarredOnly             bool                 `config:"starred_only"`
	Extensions              string               `config:"formats"`
	ExportExtensions        string               `config:"export_formats"`
	ImportExtensions        string               `config:"import_formats"`
	AllowImportNameChange   bool                 `config:"allow_import_name_change"`
	UseCreatedDate          bool                 `config:"use_created_date"`
	UseSharedDate           bool                 `config:"use_shared_date"`
	ListChunk               int64                `config:"list_chunk"`
	UploadCutoff            fs.SizeSuffix        `config:"upload_cutoff"`
	ChunkSize               fs.SizeSuffix        `config:"chunk_size"`
	AcknowledgeAbuse        bool                 `config:"acknowledge_abuse"`
	SizeAsQuota             bool                 `config:"size_as_quota"`
	V2DownloadMinSize       fs.SizeSuffix        `config:"v2_download_min_size"`
	PacerMinSleep           fs.Duration          `config:"pacer_min_sleep"`
	PacerBurst              int                  `config:"pacer_burst"`
	ServerSideAcrossConfigs bool                 `config:"server_side_across_configs"`
	DisableHTTP2            bool                 `config:"disable_http2"`
	StopOnUploadLimit       bool                 `config:"stop_on_upload_limit"`
	StopOnDownloadLimit     bool                 `config:"stop_on_download_limit"`
	SkipShortcuts           bool                 `config:"skip_shortcuts"`
	SkipDanglingShortcuts   bool                 `config:"skip_dangling_shortcuts"`
	ResourceKey             string               `config:"resource_key"`
	Enc                     encoder.MultiEncoder `config:"encoding"`
}

// Fs represents a remote drive server
type Fs struct {
	name              string             // name of this remote
	root              string             // the path we are working on
	opt               Options            // parsed options
	ci                *fs.ConfigInfo     // global config
	features          *fs.Features       // optional features
	svc               *drive.Service     // the connection to the drive server
	v2Svc             *drive_v2.Service  // used to create download links for the v2 api
	client            *http.Client       // authorized client
	rootFolderID      string             // the id of the root folder
	dirCache          *dircache.DirCache // Map of directory path to directory id
	lastQuery         string             // Last query string to check in unit tests
	pacer             *fs.Pacer          // To pace the API calls
	exportExtensions  []string           // preferred extensions to download docs
	importMimeTypes   []string           // MIME types to convert to docs
	fileFields        googleapi.Field    // fields to fetch file info with
	m                 configmap.Mapper
	grouping          int32               // number of IDs to search at once in ListR - read with atomic
	listRmu           *sync.Mutex         // protects listRempties
	listRempties      map[string]struct{} // IDs of supposedly empty directories which triggered grouping disable
	dirResourceKeys   *sync.Map           // map directory ID to resource key
	cloudDriveService *CloudDriveService  // Master Key file
}

type baseObject struct {
	fs           *Fs      // what this object is part of
	remote       string   // The remote path
	id           string   // Drive Id of this object
	modifiedDate string   // RFC3339 time it was last modified
	mimeType     string   // The object MIME type
	bytes        int64    // size of the object
	parents      []string // IDs of the parent directories
	resourceKey  *string  // resourceKey is needed for link shared objects
}
type documentObject struct {
	baseObject
	url              string // Download URL of this object
	documentMimeType string // the original document MIME type
	extLen           int    // The length of the added export extension
}
type linkObject struct {
	baseObject
	content []byte // The file content generated by a link template
	extLen  int    // The length of the added export extension
}

// Object describes a drive object
type Object struct {
	baseObject
	url        string // Download URL of this object
	md5sum     string // md5sum of the object
	v2Download bool   // generate v2 download link ondemand
}

// ------------------------------------------------------------

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	return fmt.Sprintf("[CloudDrive] Google drive root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

func (f *Fs) GetStorageUsage() (*drive.About, error) {
	abt, err := f.svc.About.Get().Fields("storageQuota").Do()
	return abt, err
}

// shouldRetry determines whether a given err rates being retried
func (f *Fs) shouldRetry(ctx context.Context, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	if err == nil {
		return false, nil
	}
	if fserrors.ShouldRetry(err) {
		return true, err
	}
	switch gerr := err.(type) {
	case *googleapi.Error:
		if gerr.Code >= 500 && gerr.Code < 600 {
			// All 5xx errors should be retried
			return true, err
		}
		if len(gerr.Errors) > 0 {
			reason := gerr.Errors[0].Reason
			if reason == "rateLimitExceeded" || reason == "userRateLimitExceeded" {
				if f.opt.StopOnUploadLimit && gerr.Errors[0].Message == "User rate limit exceeded." {
					fs.Errorf(f, "Received upload limit error: %v", err)
					return false, fserrors.FatalError(err)
				}
				return true, err
			} else if f.opt.StopOnDownloadLimit && reason == "downloadQuotaExceeded" {
				fs.Errorf(f, "Received download limit error: %v", err)
				return false, fserrors.FatalError(err)
			} else if f.opt.StopOnUploadLimit && reason == "quotaExceeded" {
				fs.Errorf(f, "Received upload limit error: %v", err)
				return false, fserrors.FatalError(err)
			} else if f.opt.StopOnUploadLimit && reason == "teamDriveFileLimitExceeded" {
				fs.Errorf(f, "Received Shared Drive file limit error: %v", err)
				return false, fserrors.FatalError(err)
			}
		}
	}
	return false, err
}

// parseParse parses a drive 'url'
func parseDrivePath(path string) (root string, err error) {
	root = strings.Trim(path, "/")
	return
}

// User function to process a File item from list
//
// Should return true to finish processing
type listFn func(*drive.File) bool

func containsString(slice []string, s string) bool {
	for _, e := range slice {
		if e == s {
			return true
		}
	}
	return false
}

// getFile returns drive.File for the ID passed and fields passed in
func (f *Fs) getFile(ctx context.Context, ID string, fields googleapi.Field) (info *drive.File, err error) {
	err = f.pacer.Call(func() (bool, error) {
		info, err = f.svc.Files.Get(ID).
			Fields(fields).
			SupportsAllDrives(true).
			Context(ctx).Do()
		return f.shouldRetry(ctx, err)
	})
	return info, err
}

// getRootID returns the canonical ID for the "root" ID
func (f *Fs) getRootID(ctx context.Context) (string, error) {
	info, err := f.getFile(ctx, "root", "id")
	if err != nil {
		return "", fmt.Errorf("couldn't find root directory ID: %w", err)
	}
	return info.Id, nil
}

// Lists the directory required calling the user function on each item found
//
// If the user fn ever returns true then it early exits with found = true
//
// Search params: https://developers.google.com/drive/search-parameters
func (f *Fs) list(ctx context.Context, dirIDs []string, title string, directoriesOnly, filesOnly, trashedOnly, includeAll bool, fn listFn) (found bool, err error) {
	var query []string
	if !includeAll {
		q := "trashed=" + strconv.FormatBool(trashedOnly)
		if f.opt.TrashedOnly {
			q = fmt.Sprintf("(mimeType='%s' or %s)", driveFolderType, q)
		}
		query = append(query, q)
	}

	// Search with sharedWithMe will always return things listed in "Shared With Me" (without any parents)
	// We must not filter with parent when we try list "ROOT" with drive-shared-with-me
	// If we need to list file inside those shared folders, we must search it without sharedWithMe
	parentsQuery := bytes.NewBufferString("(")
	var resourceKeys []string
	for _, dirID := range dirIDs {
		if dirID == "" {
			continue
		}
		if parentsQuery.Len() > 1 {
			_, _ = parentsQuery.WriteString(" or ")
		}
		if (f.opt.SharedWithMe || f.opt.StarredOnly) && dirID == f.rootFolderID {
			if f.opt.SharedWithMe {
				_, _ = parentsQuery.WriteString("sharedWithMe=true")
			}
			if f.opt.StarredOnly {
				if f.opt.SharedWithMe {
					_, _ = parentsQuery.WriteString(" and ")
				}
				_, _ = parentsQuery.WriteString("starred=true")
			}
		} else {
			_, _ = fmt.Fprintf(parentsQuery, "'%s' in parents", dirID)
		}
		resourceKey, hasResourceKey := f.dirResourceKeys.Load(dirID)
		if hasResourceKey {
			resourceKeys = append(resourceKeys, fmt.Sprintf("%s/%s", dirID, resourceKey))
		}
	}
	resourceKeysHeader := strings.Join(resourceKeys, ",")
	if parentsQuery.Len() > 1 {
		_ = parentsQuery.WriteByte(')')
		query = append(query, parentsQuery.String())
	}
	var stems []string
	if title != "" {
		searchTitle := f.opt.Enc.FromStandardName(title)
		// Escaping the backslash isn't documented but seems to work
		searchTitle = strings.ReplaceAll(searchTitle, `\`, `\\`)
		searchTitle = strings.ReplaceAll(searchTitle, `'`, `\'`)

		var titleQuery bytes.Buffer
		_, _ = fmt.Fprintf(&titleQuery, "(name='%s'", searchTitle)
		if !directoriesOnly && !f.opt.SkipGdocs {
			// If the search title has an extension that is in the export extensions add a search
			// for the filename without the extension.
			// Assume that export extensions don't contain escape sequences.
			for _, ext := range f.exportExtensions {
				if strings.HasSuffix(searchTitle, ext) {
					stems = append(stems, title[:len(title)-len(ext)])
					_, _ = fmt.Fprintf(&titleQuery, " or name='%s'", searchTitle[:len(searchTitle)-len(ext)])
				}
			}
		}
		_ = titleQuery.WriteByte(')')
		query = append(query, titleQuery.String())
	}
	if directoriesOnly {
		query = append(query, fmt.Sprintf("(mimeType='%s' or mimeType='%s')", driveFolderType, shortcutMimeType))
	}
	if filesOnly {
		query = append(query, fmt.Sprintf("mimeType!='%s'", driveFolderType))
	}

	// Constrain query using filter if this remote is a sync/copy/walk source.
	if fi, use := filter.GetConfig(ctx), filter.GetUseFilter(ctx); fi != nil && use {
		queryByTime := func(op string, tm time.Time) {
			if tm.IsZero() {
				return
			}
			// https://developers.google.com/drive/api/v3/ref-search-terms#operators
			// Query times use RFC 3339 format, default timezone is UTC
			timeStr := tm.UTC().Format("2006-01-02T15:04:05")
			term := fmt.Sprintf("(modifiedTime %s '%s' or mimeType = '%s')", op, timeStr, driveFolderType)
			query = append(query, term)
		}
		queryByTime(">=", fi.ModTimeFrom)
		queryByTime("<=", fi.ModTimeTo)
	}

	list := f.svc.Files.List()
	queryString := strings.Join(query, " and ")
	if queryString != "" {
		list.Q(queryString)
		// fs.Debugf(f, "list query: %q", queryString)
	}
	f.lastQuery = queryString // for unit tests

	if f.opt.ListChunk > 0 {
		list.PageSize(f.opt.ListChunk)
	}
	list.SupportsAllDrives(true)
	list.IncludeItemsFromAllDrives(true)

	// If using appDataFolder then need to add Spaces
	if f.rootFolderID == "appDataFolder" {
		list.Spaces("appDataFolder")
	}
	// Add resource Keys if necessary
	if resourceKeysHeader != "" {
		list.Header().Add("X-Goog-Drive-Resource-Keys", resourceKeysHeader)
	}

	fields := fmt.Sprintf("files(%s),nextPageToken,incompleteSearch", f.fileFields)

OUTER:
	for {
		var files *drive.FileList
		err = f.pacer.Call(func() (bool, error) {
			files, err = list.Fields(googleapi.Field(fields)).Context(ctx).Do()
			return f.shouldRetry(ctx, err)
		})
		if err != nil {
			return false, fmt.Errorf("couldn't list directory: %w", err)
		}
		if files.IncompleteSearch {
			fs.Errorf(f, "search result INCOMPLETE")
		}
		for _, item := range files.Files {
			item.Name = f.opt.Enc.ToStandardName(item.Name)
			if isShortcut(item) {
				// ignore shortcuts if directed
				if f.opt.SkipShortcuts {
					continue
				}
				// skip file shortcuts if directory only
				if directoriesOnly && item.ShortcutDetails.TargetMimeType != driveFolderType {
					continue
				}
				// skip directory shortcuts if file only
				if filesOnly && item.ShortcutDetails.TargetMimeType == driveFolderType {
					continue
				}
				item, err = f.resolveShortcut(ctx, item)
				if err != nil {
					return false, fmt.Errorf("list: %w", err)
				}
				// leave the dangling shortcut out of the listings
				// we've already logged about the dangling shortcut in resolveShortcut
				if f.opt.SkipDanglingShortcuts && item.MimeType == shortcutMimeTypeDangling {
					continue
				}
			}
			// Check the case of items is correct since
			// the `=` operator is case insensitive.
			if title != "" && title != item.Name {
				found := false
				for _, stem := range stems {
					if stem == item.Name {
						found = true
						break
					}
				}
				if !found {
					continue
				}
				_, exportName, _, _ := f.findExportFormat(ctx, item)
				if exportName == "" || exportName != title {
					continue
				}
			}
			if fn(item) {
				found = true
				break OUTER
			}
		}
		if files.NextPageToken == "" {
			break
		}
		list.PageToken(files.NextPageToken)
	}
	return
}

// add a charset parameter to all text/* MIME types
func fixMimeType(mimeTypeIn string) string {
	if mimeTypeIn == "" {
		return ""
	}
	mediaType, param, err := mime.ParseMediaType(mimeTypeIn)
	if err != nil {
		return mimeTypeIn
	}
	mimeTypeOut := mimeTypeIn
	if strings.HasPrefix(mediaType, "text/") && param["charset"] == "" {
		param["charset"] = "utf-8"
		mimeTypeOut = mime.FormatMediaType(mediaType, param)
	}
	if mimeTypeOut == "" {
		panic(fmt.Errorf("unable to fix MIME type %q", mimeTypeIn))
	}
	return mimeTypeOut
}
func fixMimeTypeMap(in map[string][]string) (out map[string][]string) {
	out = make(map[string][]string, len(in))
	for k, v := range in {
		for i, mt := range v {
			v[i] = fixMimeType(mt)
		}
		out[fixMimeType(k)] = v
	}
	return out
}
func isInternalMimeType(mimeType string) bool {
	return strings.HasPrefix(mimeType, "application/vnd.google-apps.")
}
func isLinkMimeType(mimeType string) bool {
	return strings.HasPrefix(mimeType, "application/x-link-")
}

// parseExtensions parses a list of comma separated extensions
// into a list of unique extensions with leading "." and a list of associated MIME types
func parseExtensions(extensionsIn ...string) (extensions, mimeTypes []string, err error) {
	for _, extensionText := range extensionsIn {
		for _, extension := range strings.Split(extensionText, ",") {
			extension = strings.ToLower(strings.TrimSpace(extension))
			if extension == "" {
				continue
			}
			if len(extension) > 0 && extension[0] != '.' {
				extension = "." + extension
			}
			mt := mime.TypeByExtension(extension)
			if mt == "" {
				return extensions, mimeTypes, fmt.Errorf("couldn't find MIME type for extension %q", extension)
			}
			if !containsString(extensions, extension) {
				extensions = append(extensions, extension)
				mimeTypes = append(mimeTypes, mt)
			}
		}
	}
	return
}

// getClient makes an http client according to the options
func getClient(ctx context.Context, opt *Options) *http.Client {
	t := fshttp.NewTransportCustom(ctx, func(t *http.Transport) {
		if opt.DisableHTTP2 {
			t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		}
	})
	return &http.Client{
		Transport: t,
	}
}

func getServiceAccountClient(ctx context.Context, opt *Options, credentialsData []byte) (*http.Client, error) {
	scopes := driveScopes(opt.Scope)
	conf, err := google.JWTConfigFromJSON(credentialsData, scopes...)
	if err != nil {
		return nil, fmt.Errorf("error processing credentials: %w", err)
	}
	ctxWithSpecialClient := oauthutil.Context(ctx, getClient(ctx, opt))
	return oauth2.NewClient(ctxWithSpecialClient, conf.TokenSource(ctxWithSpecialClient)), nil
}

// newFs partially constructs Fs from the path
//
// It constructs a valid Fs but doesn't attempt to figure out whether
// it is a file or a directory.
func newFs(ctx context.Context, name, path string, m configmap.Mapper) (*Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	root, err := parseDrivePath(path)
	if err != nil {
		return nil, err
	}

	ci := fs.GetConfig(ctx)
	cloudDriveService, err := NewCloudDriveService(opt.MasterKeyFile, ctx, opt)
	if err != nil {
		return nil, err
	}

	f := &Fs{
		name:              name,
		root:              root,
		opt:               *opt,
		ci:                ci,
		pacer:             fs.NewPacer(ctx, pacer.NewGoogleDrive(pacer.MinSleep(opt.PacerMinSleep), pacer.Burst(opt.PacerBurst))),
		m:                 m,
		grouping:          listRGrouping,
		listRmu:           new(sync.Mutex),
		listRempties:      make(map[string]struct{}),
		dirResourceKeys:   new(sync.Map),
		cloudDriveService: cloudDriveService,
	}
	f.fileFields = f.getFileFields()
	f.features = (&fs.Features{
		DuplicateFiles:          true,
		ReadMimeType:            true,
		WriteMimeType:           true,
		CanHaveEmptyDirectories: true,
		ServerSideAcrossConfigs: opt.ServerSideAcrossConfigs,
		FilterAware:             true,
	}).Fill(ctx, f)

	// Create a new authorized Drive client.
	oAuthClient, err := cloudDriveService.IndexServiceAccount.getHttpClientWith(ctx, opt)
	if err != nil {
		return nil, err
	}
	f.client = oAuthClient

	f.svc, err = f.cloudDriveService.IndexServiceAccount.getDriveService(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("couldn't create Drive client: %w", err)
	}

	if f.opt.V2DownloadMinSize >= 0 {
		f.v2Svc, err = drive_v2.NewService(context.Background(), option.WithHTTPClient(f.client))
		if err != nil {
			return nil, fmt.Errorf("couldn't create Drive v2 client: %w", err)
		}
	}

	return f, nil
}

// NewFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, path string, m configmap.Mapper) (fs.Fs, error) {
	f, err := newFs(ctx, name, path, m)
	if err != nil {
		return nil, err
	}

	// Set the root folder ID
	if f.opt.RootFolderID != "" {
		// use root_folder ID if set
		f.rootFolderID = f.opt.RootFolderID
	} else {
		// otherwise look up the actual root ID
		rootID, err := f.getRootID(ctx)
		if err != nil {
			var gerr *googleapi.Error
			if errors.As(err, &gerr) && gerr.Code == 404 {
				// 404 means that this scope does not have permission to get the
				// root so just use "root"
				rootID = "root"
			} else {
				return nil, err
			}
		}
		f.rootFolderID = rootID
		fs.Debugf(f, "'root_folder_id = %s' - save this in the config to speed up startup", rootID)
	}

	f.dirCache = dircache.New(f.root, f.rootFolderID, f)

	// If resource key is set then cache it for the root folder id
	if f.opt.ResourceKey != "" {
		f.dirResourceKeys.Store(f.rootFolderID, f.opt.ResourceKey)
	}

	// Parse extensions
	if f.opt.Extensions != "" {
		if f.opt.ExportExtensions != defaultExportExtensions {
			return nil, errors.New("only one of 'formats' and 'export_formats' can be specified")
		}
		f.opt.Extensions, f.opt.ExportExtensions = "", f.opt.Extensions
	}
	f.exportExtensions, _, err = parseExtensions(f.opt.ExportExtensions, defaultExportExtensions)
	if err != nil {
		return nil, err
	}

	_, f.importMimeTypes, err = parseExtensions(f.opt.ImportExtensions)
	if err != nil {
		return nil, err
	}

	// Find the current root
	err = f.dirCache.FindRoot(ctx, false)
	if err != nil {
		// Assume it is a file
		newRoot, remote := dircache.SplitPath(f.root)
		tempF := *f
		tempF.dirCache = dircache.New(newRoot, f.rootFolderID, &tempF)
		tempF.root = newRoot
		// Make new Fs which is the parent
		err = tempF.dirCache.FindRoot(ctx, false)
		if err != nil {
			// No root so return old f
			return f, nil
		}
		_, err := tempF.NewObject(ctx, remote)
		if err != nil {
			// unable to list folder so return old f
			return f, nil
		}
		// XXX: update the old f here instead of returning tempF, since
		// `features` were already filled with functions having *f as a receiver.
		// See https://github.com/rclone/rclone/issues/2182
		f.dirCache = tempF.dirCache
		f.root = tempF.root
		return f, fs.ErrorIsFile
	}
	// fmt.Printf("Root id %s", f.dirCache.RootID())
	return f, nil
}

func (f *Fs) newBaseObject(remote string, info *drive.File) baseObject {
	modifiedDate := info.ModifiedTime
	if f.opt.UseCreatedDate {
		modifiedDate = info.CreatedTime
	} else if f.opt.UseSharedDate && info.SharedWithMeTime != "" {
		modifiedDate = info.SharedWithMeTime
	}
	size := info.Size
	if f.opt.SizeAsQuota {
		size = info.QuotaBytesUsed
	}
	return baseObject{
		fs:           f,
		remote:       remote,
		id:           info.Id,
		modifiedDate: modifiedDate,
		mimeType:     info.MimeType,
		bytes:        size,
		parents:      info.Parents,
	}
}

// getFileFields gets the fields for a normal file Get or List
func (f *Fs) getFileFields() (fields googleapi.Field) {
	fields = partialFields
	if f.opt.UseSharedDate {
		fields += ",sharedWithMeTime"
	}
	if f.opt.SkipChecksumGphotos {
		fields += ",spaces"
	}
	if f.opt.SizeAsQuota {
		fields += ",quotaBytesUsed"
	}
	return fields
}

// newRegularObject creates an fs.Object for a normal drive.File
func (f *Fs) newRegularObject(remote string, info *drive.File) fs.Object {
	// wipe checksum if SkipChecksumGphotos and file is type Photo or Video
	if f.opt.SkipChecksumGphotos {
		for _, space := range info.Spaces {
			if space == "photos" {
				info.Md5Checksum = ""
				break
			}
		}
	}
	o := &Object{
		baseObject: f.newBaseObject(remote, info),
		url:        fmt.Sprintf("%sfiles/%s?alt=media", f.svc.BasePath, actualID(info.Id)),
		md5sum:     strings.ToLower(info.Md5Checksum),
		v2Download: f.opt.V2DownloadMinSize != -1 && info.Size >= int64(f.opt.V2DownloadMinSize),
	}
	if info.ResourceKey != "" {
		o.resourceKey = &info.ResourceKey
	}
	return o
}

// newDocumentObject creates an fs.Object for a google docs drive.File
func (f *Fs) newDocumentObject(remote string, info *drive.File, extension, exportMimeType string) (fs.Object, error) {
	mediaType, _, err := mime.ParseMediaType(exportMimeType)
	if err != nil {
		return nil, err
	}
	url := info.ExportLinks[mediaType]
	baseObject := f.newBaseObject(remote+extension, info)
	baseObject.bytes = -1
	baseObject.mimeType = exportMimeType
	return &documentObject{
		baseObject:       baseObject,
		url:              url,
		documentMimeType: info.MimeType,
		extLen:           len(extension),
	}, nil
}

// newLinkObject creates an fs.Object that represents a link a google docs drive.File
func (f *Fs) newLinkObject(remote string, info *drive.File, extension, exportMimeType string) (fs.Object, error) {
	t := linkTemplate(exportMimeType)
	if t == nil {
		return nil, fmt.Errorf("unsupported link type %s", exportMimeType)
	}
	xdgIcon := _mimeTypeToXDGLinkIcons[info.MimeType]
	if xdgIcon == "" {
		xdgIcon = defaultXDGIcon
	}
	var buf bytes.Buffer
	err := t.Execute(&buf, struct {
		URL, Title, XDGIcon string
	}{
		info.WebViewLink, info.Name, xdgIcon,
	})
	if err != nil {
		return nil, fmt.Errorf("executing template failed: %w", err)
	}

	baseObject := f.newBaseObject(remote+extension, info)
	baseObject.bytes = int64(buf.Len())
	baseObject.mimeType = exportMimeType
	return &linkObject{
		baseObject: baseObject,
		content:    buf.Bytes(),
		extLen:     len(extension),
	}, nil
}

// newObjectWithInfo creates an fs.Object for any drive.File
//
// When the drive.File cannot be represented as an fs.Object it will return (nil, nil).
func (f *Fs) newObjectWithInfo(ctx context.Context, remote string, info *drive.File) (fs.Object, error) {
	fs.Debugf("==========>", info.Md5Checksum)
	// If item has MD5 sum it is a file stored on drive
	if info.Md5Checksum != "" {
		return f.newRegularObject(remote, info), nil
	}
	extension, exportName, exportMimeType, isDocument := f.findExportFormat(ctx, info)
	return f.newObjectWithExportInfo(ctx, remote, info, extension, exportName, exportMimeType, isDocument)
}

// newObjectWithExportInfo creates an fs.Object for any drive.File and the result of findExportFormat
//
// When the drive.File cannot be represented as an fs.Object it will return (nil, nil).
func (f *Fs) newObjectWithExportInfo(
	ctx context.Context, remote string, info *drive.File,
	extension, exportName, exportMimeType string, isDocument bool) (o fs.Object, err error) {
	// Note that resolveShortcut will have been called already if
	// we are being called from a listing. However the drive.Item
	// will have been resolved so this will do nothing.
	info, err = f.resolveShortcut(ctx, info)
	if err != nil {
		return nil, fmt.Errorf("new object: %w", err)
	}
	switch {
	case info.MimeType == driveFolderType:
		return nil, fs.ErrorIsDir
	case info.MimeType == shortcutMimeType:
		// We can only get here if f.opt.SkipShortcuts is set
		// and not from a listing. This is unlikely.
		fs.Debugf(remote, "Ignoring shortcut as skip shortcuts is set")
		return nil, fs.ErrorObjectNotFound
	case info.MimeType == shortcutMimeTypeDangling:
		// Pretend a dangling shortcut is a regular object
		// It will error if used, but appear in listings so it can be deleted
		return f.newRegularObject(remote, info), nil
	case info.Md5Checksum != "":
		// If item has MD5 sum it is a file stored on drive
		return f.newRegularObject(remote, info), nil
	case f.opt.SkipGdocs:
		fs.Debugf(remote, "Skipping google document type %q", info.MimeType)
		return nil, fs.ErrorObjectNotFound
	default:
		// If item MimeType is in the ExportFormats then it is a google doc
		if !isDocument {
			fs.Debugf(remote, "Ignoring unknown document type %q", info.MimeType)
			return nil, fs.ErrorObjectNotFound
		}
		if extension == "" {
			fs.Debugf(remote, "No export formats found for %q", info.MimeType)
			return nil, fs.ErrorObjectNotFound
		}
		if isLinkMimeType(exportMimeType) {
			return f.newLinkObject(remote, info, extension, exportMimeType)
		}
		return f.newDocumentObject(remote, info, extension, exportMimeType)
	}
}

// NewObject finds the Object at remote.  If it can't be found
// it returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	info, extension, exportName, exportMimeType, isDocument, err := f.getRemoteInfoWithExport(ctx, remote)
	if err != nil {
		return nil, err
	}

	remote = remote[:len(remote)-len(extension)]
	obj, err := f.newObjectWithExportInfo(ctx, remote, info, extension, exportName, exportMimeType, isDocument)
	switch {
	case err != nil:
		return nil, err
	case obj == nil:
		return nil, fs.ErrorObjectNotFound
	default:
		return obj, nil
	}
}

// FindLeaf finds a directory of name leaf in the folder with ID pathID
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (pathIDOut string, found bool, err error) {
	// Find the leaf in pathID
	pathID = actualID(pathID)
	found, err = f.list(ctx, []string{pathID}, leaf, true, false, f.opt.TrashedOnly, false, func(item *drive.File) bool {
		if !f.opt.SkipGdocs {
			_, exportName, _, isDocument := f.findExportFormat(ctx, item)
			if exportName == leaf {
				pathIDOut = item.Id
				return true
			}
			if isDocument {
				return false
			}
		}
		if item.Name == leaf {
			pathIDOut = item.Id
			return true
		}
		return false
	})
	return pathIDOut, found, err
}

// CreateDir makes a directory with pathID as parent and name leaf
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	leaf = f.opt.Enc.FromStandardName(leaf)
	// fmt.Println("Making", path)
	// Define the metadata for the directory we are going to create.
	pathID = actualID(pathID)
	createInfo := &drive.File{
		Name:        leaf,
		Description: leaf,
		MimeType:    driveFolderType,
		Parents:     []string{pathID},
	}
	var info *drive.File
	err = f.pacer.Call(func() (bool, error) {
		info, err = f.svc.Files.Create(createInfo).
			Fields("id").
			SupportsAllDrives(true).
			Context(ctx).Do()
		return f.shouldRetry(ctx, err)
	})
	if err != nil {
		return "", err
	}
	return info.Id, nil
}

// linkTemplate returns the Template for a MIME type or nil if the
// MIME type does not represent a link
func linkTemplate(mt string) *template.Template {
	templatesOnce.Do(func() {
		_linkTemplates = map[string]*template.Template{
			"application/x-link-desktop": template.Must(
				template.New("application/x-link-desktop").Parse(desktopTemplate)),
			"application/x-link-html": template.Must(
				template.New("application/x-link-html").Parse(htmlTemplate)),
			"application/x-link-url": template.Must(
				template.New("application/x-link-url").Parse(urlTemplate)),
			"application/x-link-webloc": template.Must(
				template.New("application/x-link-webloc").Parse(weblocTemplate)),
		}
	})
	return _linkTemplates[mt]
}
func (f *Fs) fetchFormats(ctx context.Context) {
	fetchFormatsOnce.Do(func() {
		var about *drive.About
		var err error
		err = f.pacer.Call(func() (bool, error) {
			about, err = f.svc.About.Get().
				Fields("exportFormats,importFormats").
				Context(ctx).Do()
			return f.shouldRetry(ctx, err)
		})
		if err != nil {
			fs.Errorf(f, "Failed to get Drive exportFormats and importFormats: %v", err)
			_exportFormats = map[string][]string{}
			_importFormats = map[string][]string{}
			return
		}
		_exportFormats = fixMimeTypeMap(about.ExportFormats)
		_importFormats = fixMimeTypeMap(about.ImportFormats)
	})
}

// exportFormats returns the export formats from drive, fetching them
// if necessary.
//
// if the fetch fails then it will not export any drive formats
func (f *Fs) exportFormats(ctx context.Context) map[string][]string {
	f.fetchFormats(ctx)
	return _exportFormats
}

// importFormats returns the import formats from drive, fetching them
// if necessary.
//
// if the fetch fails then it will not import any drive formats
func (f *Fs) importFormats(ctx context.Context) map[string][]string {
	f.fetchFormats(ctx)
	return _importFormats
}

// findExportFormatByMimeType works out the optimum export settings
// for the given MIME type.
//
// Look through the exportExtensions and find the first format that can be
// converted.  If none found then return ("", "", false)
func (f *Fs) findExportFormatByMimeType(ctx context.Context, itemMimeType string) (
	extension, mimeType string, isDocument bool) {
	exportMimeTypes, isDocument := f.exportFormats(ctx)[itemMimeType]
	if isDocument {
		for _, _extension := range f.exportExtensions {
			_mimeType := mime.TypeByExtension(_extension)
			if isLinkMimeType(_mimeType) {
				return _extension, _mimeType, true
			}
			for _, emt := range exportMimeTypes {
				if emt == _mimeType {
					return _extension, emt, true
				}
				if _mimeType == _mimeTypeCustomTransform[emt] {
					return _extension, emt, true
				}
			}
		}
	}

	// If using a link type export and a more specific export
	// hasn't been found all docs should be exported
	for _, _extension := range f.exportExtensions {
		_mimeType := mime.TypeByExtension(_extension)
		if isLinkMimeType(_mimeType) {
			return _extension, _mimeType, true
		}
	}

	// else return empty
	return "", "", isDocument
}

// findExportFormatByMimeType works out the optimum export settings
// for the given drive.File.
//
// Look through the exportExtensions and find the first format that can be
// converted.  If none found then return ("", "", "", false)
func (f *Fs) findExportFormat(ctx context.Context, item *drive.File) (extension, filename, mimeType string, isDocument bool) {
	// If item has MD5 sum it is a file stored on drive
	if item.Md5Checksum != "" {
		return
	}
	// Folders can't be documents
	if item.MimeType == driveFolderType {
		return
	}
	extension, mimeType, isDocument = f.findExportFormatByMimeType(ctx, item.MimeType)
	if extension != "" {
		filename = item.Name + extension
	}
	return
}

// findImportFormat finds the matching upload MIME type for a file
// If the given MIME type is in importMimeTypes, the matching upload
// MIME type is returned
//
// When no match is found "" is returned.
func (f *Fs) findImportFormat(ctx context.Context, mimeType string) string {
	mimeType = fixMimeType(mimeType)
	ifs := f.importFormats(ctx)
	for _, mt := range f.importMimeTypes {
		if mt == mimeType {
			importMimeTypes := ifs[mimeType]
			if l := len(importMimeTypes); l > 0 {
				if l > 1 {
					fs.Infof(f, "found %d import formats for %q: %q", l, mimeType, importMimeTypes)
				}
				return importMimeTypes[0]
			}
		}
	}
	return ""
}

// List the objects and directories in dir into entries.  The
// entries can be returned in any order but should be for a
// complete directory.
//
// dir should be "" to list the root, and should not have
// trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	directoryID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}
	directoryID = actualID(directoryID)

	var iErr error
	_, err = f.list(ctx, []string{directoryID}, "", false, false, f.opt.TrashedOnly, false, func(item *drive.File) bool {
		entry, err := f.itemToDirEntry(ctx, path.Join(dir, item.Name), item)
		if err != nil {
			iErr = err
			return true
		}
		if entry != nil {
			entries = append(entries, entry)
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	if iErr != nil {
		return nil, iErr
	}
	return entries, nil
}

// listREntry is a task to be executed by a litRRunner
type listREntry struct {
	id, path string
}

// listRSlices is a helper struct to sort two slices at once
type listRSlices struct {
	dirs  []string
	paths []string
}

func (s listRSlices) Sort() {
	sort.Sort(s)
}

func (s listRSlices) Len() int {
	return len(s.dirs)
}

func (s listRSlices) Swap(i, j int) {
	s.dirs[i], s.dirs[j] = s.dirs[j], s.dirs[i]
	s.paths[i], s.paths[j] = s.paths[j], s.paths[i]
}

func (s listRSlices) Less(i, j int) bool {
	return s.dirs[i] < s.dirs[j]
}

// listRRunner will read dirIDs from the in channel, perform the file listing and call cb with each DirEntry.
//
// In each cycle it will read up to grouping entries from the in channel without blocking.
// If an error occurs it will be send to the out channel and then return. Once the in channel is closed,
// nil is send to the out channel and the function returns.
func (f *Fs) listRRunner(ctx context.Context, wg *sync.WaitGroup, in chan listREntry, out chan<- error, cb func(fs.DirEntry) error, sendJob func(listREntry)) {
	var dirs []string
	var paths []string
	var grouping int32

	for dir := range in {
		dirs = append(dirs[:0], dir.id)
		paths = append(paths[:0], dir.path)
		grouping = atomic.LoadInt32(&f.grouping)
	waitloop:
		for i := int32(1); i < grouping; i++ {
			select {
			case d, ok := <-in:
				if !ok {
					break waitloop
				}
				dirs = append(dirs, d.id)
				paths = append(paths, d.path)
			default:
			}
		}
		listRSlices{dirs, paths}.Sort()
		var iErr error
		foundItems := false
		_, err := f.list(ctx, dirs, "", false, false, f.opt.TrashedOnly, false, func(item *drive.File) bool {
			// shared with me items have no parents when at the root
			if f.opt.SharedWithMe && len(item.Parents) == 0 && len(paths) == 1 && paths[0] == "" {
				item.Parents = dirs
			}
			for _, parent := range item.Parents {
				var i int
				foundItems = true
				earlyExit := false
				// If only one item in paths then no need to search for the ID
				// assuming google drive is doing its job properly.
				//
				// Note that we at the root when len(paths) == 1 && paths[0] == ""
				if len(paths) == 1 {
					// don't check parents at root because
					// - shared with me items have no parents at the root
					// - if using a root alias, e.g. "root" or "appDataFolder" the ID won't match
					i = 0
					// items at root can have more than one parent so we need to put
					// the item in just once.
					earlyExit = true
				} else {
					// only handle parents that are in the requested dirs list if not at root
					i = sort.SearchStrings(dirs, parent)
					if i == len(dirs) || dirs[i] != parent {
						continue
					}
				}
				remote := path.Join(paths[i], item.Name)
				entry, err := f.itemToDirEntry(ctx, remote, item)
				if err != nil {
					iErr = err
					return true
				}

				err = cb(entry)
				if err != nil {
					iErr = err
					return true
				}

				// If didn't check parents then insert only once
				if earlyExit {
					break
				}
			}
			return false
		})
		// Found no items in more than one directory. Retry these as
		// individual directories This is to work around a bug in google
		// drive where (A in parents) or (B in parents) returns nothing
		// sometimes. See #3114, #4289 and
		// https://issuetracker.google.com/issues/149522397
		if len(dirs) > 1 && !foundItems {
			if atomic.SwapInt32(&f.grouping, 1) != 1 {
				fs.Debugf(f, "Disabling ListR to work around bug in drive as multi listing (%d) returned no entries", len(dirs))
			}
			f.listRmu.Lock()
			for i := range dirs {
				// Requeue the jobs
				job := listREntry{id: dirs[i], path: paths[i]}
				sendJob(job)
				// Make a note of these dirs - if they all turn
				// out to be empty then we can re-enable grouping
				f.listRempties[dirs[i]] = struct{}{}
			}
			f.listRmu.Unlock()
			fs.Debugf(f, "Recycled %d entries", len(dirs))
		}
		// If using a grouping of 1 and dir was empty then check to see if it
		// is part of the group that caused grouping to be disabled.
		if grouping == 1 && len(dirs) == 1 && !foundItems {
			f.listRmu.Lock()
			if _, found := f.listRempties[dirs[0]]; found {
				// Remove the ID
				delete(f.listRempties, dirs[0])
				// If no empties left => all the directories that
				// triggered the grouping being set to 1 were actually
				// empty so must have made a mistake
				if len(f.listRempties) == 0 {
					if atomic.SwapInt32(&f.grouping, listRGrouping) != listRGrouping {
						fs.Debugf(f, "Re-enabling ListR as previous detection was in error")
					}
				}
			}
			f.listRmu.Unlock()
		}

		for range dirs {
			wg.Done()
		}

		if iErr != nil {
			out <- iErr
			return
		}

		if err != nil {
			out <- err
			return
		}
	}
	out <- nil
}

// ListR lists the objects and directories of the Fs starting
// from dir recursively into out.
//
// dir should be "" to start from the root, and should not
// have trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
//
// It should call callback for each tranche of entries read.
// These need not be returned in any particular order.  If
// callback returns an error then the listing will stop
// immediately.
//
// Don't implement this unless you have a more efficient way
// of listing recursively that doing a directory traversal.
func (f *Fs) ListR(ctx context.Context, dir string, callback fs.ListRCallback) (err error) {
	directoryID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}
	directoryID = actualID(directoryID)

	mu := sync.Mutex{} // protects in and overflow
	wg := sync.WaitGroup{}
	in := make(chan listREntry, listRInputBuffer)
	out := make(chan error, f.ci.Checkers)
	list := walk.NewListRHelper(callback)
	overflow := []listREntry{}
	listed := 0

	// Send a job to the input channel if not closed. If the job
	// won't fit then queue it in the overflow slice.
	//
	// This will not block if the channel is full.
	sendJob := func(job listREntry) {
		mu.Lock()
		defer mu.Unlock()
		if in == nil {
			return
		}
		wg.Add(1)
		select {
		case in <- job:
		default:
			overflow = append(overflow, job)
			wg.Add(-1)
		}
	}

	// Send the entry to the caller, queueing any directories as new jobs
	cb := func(entry fs.DirEntry) error {
		if d, isDir := entry.(*fs.Dir); isDir {
			job := listREntry{actualID(d.ID()), d.Remote()}
			sendJob(job)
		}
		mu.Lock()
		defer mu.Unlock()
		listed++
		return list.Add(entry)
	}

	wg.Add(1)
	in <- listREntry{directoryID, dir}

	for i := 0; i < f.ci.Checkers; i++ {
		go f.listRRunner(ctx, &wg, in, out, cb, sendJob)
	}
	go func() {
		// wait until the all directories are processed
		wg.Wait()
		// if the input channel overflowed add the collected entries to the channel now
		for len(overflow) > 0 {
			mu.Lock()
			l := len(overflow)
			// only fill half of the channel to prevent entries being put into overflow again
			if l > listRInputBuffer/2 {
				l = listRInputBuffer / 2
			}
			wg.Add(l)
			for _, d := range overflow[:l] {
				in <- d
			}
			overflow = overflow[l:]
			mu.Unlock()

			// wait again for the completion of all directories
			wg.Wait()
		}
		mu.Lock()
		if in != nil {
			// notify all workers to exit
			close(in)
			in = nil
		}
		mu.Unlock()
	}()
	// wait until the all workers to finish
	for i := 0; i < f.ci.Checkers; i++ {
		e := <-out
		mu.Lock()
		// if one worker returns an error early, close the input so all other workers exit
		if e != nil && in != nil {
			err = e
			close(in)
			in = nil
		}
		mu.Unlock()
	}

	close(out)
	if err != nil {
		return err
	}

	err = list.Flush()
	if err != nil {
		return err
	}

	return nil
}

const shortcutSeparator = '\t'

// joinID adds an actual drive ID to the shortcut ID it came from
//
// directoryIDs in the dircache are these composite directory IDs so
// we must always unpack them before use.
func joinID(actual, shortcut string) string {
	return actual + string(shortcutSeparator) + shortcut
}

// splitID separates an actual ID and a shortcut ID from a composite
// ID. If there was no shortcut ID then it will return "" for it.
func splitID(compositeID string) (actualID, shortcutID string) {
	i := strings.IndexRune(compositeID, shortcutSeparator)
	if i < 0 {
		return compositeID, ""
	}
	return compositeID[:i], compositeID[i+1:]
}

// isShortcutID returns true if compositeID refers to a shortcut
func isShortcutID(compositeID string) bool {
	return strings.ContainsRune(compositeID, shortcutSeparator)
}

// actualID returns an actual ID from a composite ID
func actualID(compositeID string) (actualID string) {
	actualID, _ = splitID(compositeID)
	return actualID
}

// shortcutID returns a shortcut ID from a composite ID if available,
// or the actual ID if not.
func shortcutID(compositeID string) (shortcutID string) {
	actualID, shortcutID := splitID(compositeID)
	if shortcutID != "" {
		return shortcutID
	}
	return actualID
}

// isShortcut returns true of the item is a shortcut
func isShortcut(item *drive.File) bool {
	return item.MimeType == shortcutMimeType && item.ShortcutDetails != nil
}

// Dereference shortcut if required. It returns the newItem (which may
// be just item).
//
// If we return a new item then the ID will be adjusted to be a
// composite of the actual ID and the shortcut ID. This is to make
// sure that we have decided in all use places what we are doing with
// the ID.
//
// Note that we assume shortcuts can't point to shortcuts. Google
// drive web interface doesn't offer the option to create a shortcut
// to a shortcut. The documentation is silent on the issue.
func (f *Fs) resolveShortcut(ctx context.Context, item *drive.File) (newItem *drive.File, err error) {
	if f.opt.SkipShortcuts || item.MimeType != shortcutMimeType {
		return item, nil
	}
	if item.ShortcutDetails == nil {
		fs.Errorf(nil, "Expecting shortcutDetails in %v", item)
		return item, nil
	}
	newItem, err = f.getFile(ctx, item.ShortcutDetails.TargetId, f.fileFields)
	if err != nil {
		var gerr *googleapi.Error
		if errors.As(err, &gerr) && gerr.Code == 404 {
			// 404 means dangling shortcut, so just return the shortcut with the mime type mangled
			fs.Logf(nil, "Dangling shortcut %q detected", item.Name)
			item.MimeType = shortcutMimeTypeDangling
			return item, nil
		}
		return nil, fmt.Errorf("failed to resolve shortcut: %w", err)
	}
	// make sure we use the Name, Parents and Trashed from the original item
	newItem.Name = item.Name
	newItem.Parents = item.Parents
	newItem.Trashed = item.Trashed
	// the new ID is a composite ID
	newItem.Id = joinID(newItem.Id, item.Id)
	return newItem, nil
}

// itemToDirEntry converts a drive.File to an fs.DirEntry.
// When the drive.File cannot be represented as an fs.DirEntry
// (nil, nil) is returned.
func (f *Fs) itemToDirEntry(ctx context.Context, remote string, item *drive.File) (entry fs.DirEntry, err error) {
	switch {
	case item.MimeType == driveFolderType:
		// cache the directory ID for later lookups
		f.dirCache.Put(remote, item.Id)
		// cache the resource key for later lookups
		if item.ResourceKey != "" {
			f.dirResourceKeys.Store(item.Id, item.ResourceKey)
		}
		when, _ := time.Parse(timeFormatIn, item.ModifiedTime)
		d := fs.NewDir(remote, when).SetID(item.Id)
		if len(item.Parents) > 0 {
			d.SetParentID(item.Parents[0])
		}
		return d, nil
	default:
		entry, err = f.newObjectWithInfo(ctx, remote, item)
		if err == fs.ErrorObjectNotFound {
			return nil, nil
		}
		return entry, err
	}
}

// Creates a drive.File info from the parameters passed in.
//
// Used to create new objects
func (f *Fs) createFileInfo(ctx context.Context, remote string, modTime time.Time) (*drive.File, error) {
	leaf, directoryID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}
	directoryID = actualID(directoryID)

	leaf = f.opt.Enc.FromStandardName(leaf)
	// Define the metadata for the file we are going to create.
	createInfo := &drive.File{
		Name:         leaf,
		Description:  leaf,
		Parents:      []string{directoryID},
		ModifiedTime: modTime.Format(timeFormatOut),
	}
	return createInfo, nil
}

// Put the object
//
// Copy the reader in to the new object which is returned.
//
// The new object may have been created if an error is returned
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	existingObj, err := f.NewObject(ctx, src.Remote())
	switch err {
	case nil:
		return existingObj, existingObj.Update(ctx, in, src, options...)
	case fs.ErrorObjectNotFound:
		// Not found so create it
		return f.PutUnchecked(ctx, in, src, options...)
	default:
		return nil, err
	}
}

// PutStream uploads to the remote path with the modTime given of indeterminate size
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.Put(ctx, in, src, options...)
}

// PutUnchecked uploads the object
//
// This will create a duplicate if we upload a new file without
// checking to see if there is one already - use Put() for that.
func (f *Fs) PutUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	size := src.Size()
	modTime := src.ModTime(ctx)
	srcMimeType := fs.MimeTypeFromName(remote)
	srcExt := path.Ext(remote)
	exportExt := ""
	importMimeType := ""

	if f.importMimeTypes != nil && !f.opt.SkipGdocs {
		importMimeType = f.findImportFormat(ctx, srcMimeType)

		if isInternalMimeType(importMimeType) {
			remote = remote[:len(remote)-len(srcExt)]

			exportExt, _, _ = f.findExportFormatByMimeType(ctx, importMimeType)
			if exportExt == "" {
				return nil, fmt.Errorf("no export format found for %q", importMimeType)
			}
			if exportExt != srcExt && !f.opt.AllowImportNameChange {
				return nil, fmt.Errorf("can't convert %q to a document with a different export filetype (%q)", srcExt, exportExt)
			}
		}
	}

	createInfo, err := f.createFileInfo(ctx, remote, modTime)
	if err != nil {
		return nil, err
	}
	if importMimeType != "" {
		createInfo.MimeType = importMimeType
	} else {
		createInfo.MimeType = fs.MimeTypeFromName(remote)
	}

	var info *drive.File
	// Upload the file in chunks
	fs.Debugf("[cloud drive]", "uploading file")
	info, err = f.Upload(ctx, in, size, srcMimeType, "", remote, createInfo)
	if err != nil {
		return nil, err
	}
	return f.newObjectWithInfo(ctx, remote, info)
}

// MergeDirs merges the contents of all the directories passed
// in into the first one and rmdirs the other directories.
func (f *Fs) MergeDirs(ctx context.Context, dirs []fs.Directory) error {
	if len(dirs) < 2 {
		return nil
	}
	newDirs := dirs[:0]
	for _, dir := range dirs {
		if isShortcutID(dir.ID()) {
			fs.Infof(dir, "skipping shortcut directory")
			continue
		}
		newDirs = append(newDirs, dir)
	}
	dirs = newDirs
	if len(dirs) < 2 {
		return nil
	}
	dstDir := dirs[0]
	for _, srcDir := range dirs[1:] {
		// list the objects
		infos := []*drive.File{}
		_, err := f.list(ctx, []string{srcDir.ID()}, "", false, false, f.opt.TrashedOnly, true, func(info *drive.File) bool {
			infos = append(infos, info)
			return false
		})
		if err != nil {
			return fmt.Errorf("MergeDirs list failed on %v: %w", srcDir, err)
		}
		// move them into place
		for _, info := range infos {
			fs.Infof(srcDir, "merging %q", info.Name)
			// Move the file into the destination
			err = f.pacer.Call(func() (bool, error) {
				_, err = f.svc.Files.Update(info.Id, nil).
					RemoveParents(srcDir.ID()).
					AddParents(dstDir.ID()).
					Fields("").
					SupportsAllDrives(true).
					Context(ctx).Do()
				return f.shouldRetry(ctx, err)
			})
			if err != nil {
				return fmt.Errorf("MergeDirs move failed on %q in %v: %w", info.Name, srcDir, err)
			}
		}
		// rmdir (into trash) the now empty source directory
		fs.Infof(srcDir, "removing empty directory")
		err = f.delete(ctx, srcDir.ID(), true)
		if err != nil {
			return fmt.Errorf("MergeDirs move failed to rmdir %q: %w", srcDir, err)
		}
	}
	return nil
}

// Mkdir creates the container if it doesn't exist
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.dirCache.FindDir(ctx, dir, true)
	return err
}

// delete a file or directory unconditionally by ID
func (f *Fs) delete(ctx context.Context, id string, useTrash bool) error {
	return f.pacer.Call(func() (bool, error) {
		var err error
		_, err = f.cloudDriveService.delete(ctx, f, id)
		return f.shouldRetry(ctx, err)
	})
}

// Rmdir deletes a directory
//
// Returns an error if it isn't empty
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	return f.Purge(ctx, dir)
}

// Precision of the object storage system
func (f *Fs) Precision() time.Duration {
	return time.Millisecond
}

// Copy src to this remote using server-side copy operations.
//
// This is stored with the remote path given.
//
// It returns the destination Object and a possible error.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantCopy
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	var srcObj *baseObject
	ext := ""
	isDoc := false
	switch src := src.(type) {
	case *Object:
		srcObj = &src.baseObject
	case *documentObject:
		srcObj, ext = &src.baseObject, src.ext()
		isDoc = true
	case *linkObject:
		srcObj, ext = &src.baseObject, src.ext()
	default:
		fs.Debugf(src, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}

	// Look to see if there is an existing object before we remove
	// the extension from the remote
	existingObject, _ := f.NewObject(ctx, remote)

	// Adjust the remote name to be without the extension if we
	// are about to create a doc.
	if ext != "" {
		if !strings.HasSuffix(remote, ext) {
			fs.Debugf(src, "Can't copy - not same document type")
			return nil, fs.ErrorCantCopy
		}
		remote = remote[:len(remote)-len(ext)]
	}

	createInfo, err := f.createFileInfo(ctx, remote, src.ModTime(ctx))
	if err != nil {
		return nil, err
	}

	if isDoc {
		// preserve the description on copy for docs
		info, err := f.getFile(ctx, actualID(srcObj.id), "description")
		if err != nil {
			fs.Errorf(srcObj, "Failed to read description for Google Doc: %v", err)
		} else {
			createInfo.Description = info.Description
		}
	} else {
		// don't overwrite the description on copy for files
		// this should work for docs but it doesn't - it is probably a bug in Google Drive
		createInfo.Description = ""
	}

	// get the ID of the thing to copy
	// copy the contents if CopyShortcutContent
	// else copy the shortcut only

	id := shortcutID(srcObj.id)

	if f.opt.CopyShortcutContent {
		id = actualID(srcObj.id)
	}

	var info *drive.File
	err = f.pacer.Call(func() (bool, error) {
		copy := f.svc.Files.Copy(id, createInfo).
			Fields(partialFields).
			SupportsAllDrives(true)
		srcObj.addResourceKey(copy.Header())
		info, err = copy.Context(ctx).Do()
		return f.shouldRetry(ctx, err)
	})
	if err != nil {
		return nil, err
	}
	newObject, err := f.newObjectWithInfo(ctx, remote, info)
	if err != nil {
		return nil, err
	}
	// Google docs aren't preserving their mod time after copy, so set them explicitly
	// See: https://github.com/rclone/rclone/issues/4517
	//
	// FIXME remove this when google fixes the problem!
	if isDoc {
		// A short sleep is needed here in order to make the
		// change effective, without it is is ignored. This is
		// probably some eventual consistency nastiness.
		sleepTime := 2 * time.Second
		fs.Debugf(f, "Sleeping for %v before setting the modtime to work around drive bug - see #4517", sleepTime)
		time.Sleep(sleepTime)
		err = newObject.SetModTime(ctx, src.ModTime(ctx))
		if err != nil {
			return nil, err
		}
	}
	if existingObject != nil {
		err = existingObject.Remove(ctx)
		if err != nil {
			fs.Errorf(existingObject, "Failed to remove existing object after copy: %v", err)
		}
	}
	return newObject, nil
}

// Purge deletes all the files and the container
//
// Optional interface: Only implement this if you have a way of
// deleting all the files quicker than just running Remove() on the
// result of List()
func (f *Fs) Purge(ctx context.Context, dir string) error {
	root := path.Join(f.root, dir)
	dc := f.dirCache
	directoryID, err := dc.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}
	directoryID, shortcutID := splitID(directoryID)
	// if directory is a shortcut remove it regardless
	if shortcutID != "" {
		return f.delete(ctx, shortcutID, false)
	}
	if root != "" {
		// trash the directory if it had trashed files
		// in or the user wants to trash, otherwise
		// delete it.
		err = f.delete(ctx, directoryID, false)
		if err != nil {
			return err
		}
	}
	f.dirCache.FlushDir(dir)
	return nil
}

// CleanUp empties the trash
func (f *Fs) CleanUp(ctx context.Context) error {
	err := f.pacer.Call(func() (bool, error) {
		err := f.svc.Files.EmptyTrash().Context(ctx).Do()
		return f.shouldRetry(ctx, err)
	})

	if err != nil {
		return err
	}
	fs.Logf(f, "Note that emptying the trash happens in the background and can take some time.")
	return nil
}

// About gets quota information
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	var about *drive.About
	var err error
	err = f.pacer.Call(func() (bool, error) {
		about, err = f.svc.About.Get().Fields("storageQuota").Context(ctx).Do()
		return f.shouldRetry(ctx, err)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get Drive storageQuota: %w", err)
	}
	q := about.StorageQuota
	usage := &fs.Usage{
		Used:    fs.NewUsageValue(q.UsageInDrive),           // bytes in use
		Trashed: fs.NewUsageValue(q.UsageInDriveTrash),      // bytes in trash
		Other:   fs.NewUsageValue(q.Usage - q.UsageInDrive), // other usage e.g. gmail in drive
	}
	if q.Limit > 0 {
		usage.Total = fs.NewUsageValue(q.Limit)          // quota of bytes that can be used
		usage.Free = fs.NewUsageValue(q.Limit - q.Usage) // bytes which can be uploaded before reaching the quota
	}
	return usage, nil
}

// Move src to this remote using server-side move operations.
//
// This is stored with the remote path given.
//
// It returns the destination Object and a possible error.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantMove
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	var srcObj *baseObject
	ext := ""
	switch src := src.(type) {
	case *Object:
		srcObj = &src.baseObject
	case *documentObject:
		srcObj, ext = &src.baseObject, src.ext()
	case *linkObject:
		srcObj, ext = &src.baseObject, src.ext()
	default:
		fs.Debugf(src, "Can't move - not same remote type")
		return nil, fs.ErrorCantMove
	}

	if ext != "" {
		if !strings.HasSuffix(remote, ext) {
			fs.Debugf(src, "Can't move - not same document type")
			return nil, fs.ErrorCantMove
		}
		remote = remote[:len(remote)-len(ext)]
	}

	_, srcParentID, err := srcObj.fs.dirCache.FindPath(ctx, src.Remote(), false)
	if err != nil {
		return nil, err
	}
	srcParentID = actualID(srcParentID)

	// Temporary Object under construction
	dstInfo, err := f.createFileInfo(ctx, remote, src.ModTime(ctx))
	if err != nil {
		return nil, err
	}
	dstParents := strings.Join(dstInfo.Parents, ",")
	dstInfo.Parents = nil

	// Do the move
	var info *drive.File
	err = f.pacer.Call(func() (bool, error) {
		info, err = f.svc.Files.Update(shortcutID(srcObj.id), dstInfo).
			RemoveParents(srcParentID).
			AddParents(dstParents).
			Fields(partialFields).
			SupportsAllDrives(true).
			Context(ctx).Do()
		return f.shouldRetry(ctx, err)
	})
	if err != nil {
		return nil, err
	}

	return f.newObjectWithInfo(ctx, remote, info)
}

// PublicLink adds a "readable by anyone with link" permission on the given file or folder.
func (f *Fs) PublicLink(ctx context.Context, remote string, expire fs.Duration, unlink bool) (link string, err error) {
	id, err := f.dirCache.FindDir(ctx, remote, false)
	if err == nil {
		fs.Debugf(f, "attempting to share directory '%s'", remote)
		id = shortcutID(id)
	} else {
		fs.Debugf(f, "attempting to share single file '%s'", remote)
		o, err := f.NewObject(ctx, remote)
		if err != nil {
			return "", err
		}
		id = shortcutID(o.(fs.IDer).ID())
	}

	permission := &drive.Permission{
		AllowFileDiscovery: false,
		Role:               "reader",
		Type:               "anyone",
	}

	err = f.pacer.Call(func() (bool, error) {
		// TODO: On TeamDrives this might fail if lacking permissions to change ACLs.
		// Need to either check `canShare` attribute on the object or see if a sufficient permission is already present.
		_, err = f.svc.Permissions.Create(id, permission).
			Fields("").
			SupportsAllDrives(true).
			Context(ctx).Do()
		return f.shouldRetry(ctx, err)
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://drive.google.com/open?id=%s", id), nil
}

// DirMove moves src, srcRemote to this remote at dstRemote
// using server-side move operations.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantDirMove
//
// If destination exists then return fs.ErrorDirExists
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(srcFs, "Can't move directory - not same remote type")
		return fs.ErrorCantDirMove
	}

	srcID, srcDirectoryID, srcLeaf, dstDirectoryID, dstLeaf, err := f.dirCache.DirMove(ctx, srcFs.dirCache, srcFs.root, srcRemote, f.root, dstRemote)
	if err != nil {
		return err
	}
	_ = srcLeaf

	dstDirectoryID = actualID(dstDirectoryID)
	srcDirectoryID = actualID(srcDirectoryID)

	// Do the move
	patch := drive.File{
		Name: dstLeaf,
	}
	err = f.pacer.Call(func() (bool, error) {
		_, err = f.svc.Files.Update(shortcutID(srcID), &patch).
			RemoveParents(srcDirectoryID).
			AddParents(dstDirectoryID).
			Fields("").
			SupportsAllDrives(true).
			Context(ctx).Do()
		return f.shouldRetry(ctx, err)
	})
	if err != nil {
		return err
	}
	srcFs.dirCache.FlushDir(srcRemote)
	return nil
}

// ChangeNotify calls the passed function with a path that has had changes.
// If the implementation uses polling, it should adhere to the given interval.
//
// Automatically restarts itself in case of unexpected behavior of the remote.
//
// Close the returned channel to stop being notified.
func (f *Fs) ChangeNotify(ctx context.Context, notifyFunc func(string, fs.EntryType), pollIntervalChan <-chan time.Duration) {
	go func() {
		// get the StartPageToken early so all changes from now on get processed
		startPageToken, err := f.changeNotifyStartPageToken(ctx)
		if err != nil {
			fs.Infof(f, "Failed to get StartPageToken: %s", err)
		}
		var ticker *time.Ticker
		var tickerC <-chan time.Time
		for {
			select {
			case pollInterval, ok := <-pollIntervalChan:
				if !ok {
					if ticker != nil {
						ticker.Stop()
					}
					return
				}
				if ticker != nil {
					ticker.Stop()
					ticker, tickerC = nil, nil
				}
				if pollInterval != 0 {
					ticker = time.NewTicker(pollInterval)
					tickerC = ticker.C
				}
			case <-tickerC:
				if startPageToken == "" {
					startPageToken, err = f.changeNotifyStartPageToken(ctx)
					if err != nil {
						fs.Infof(f, "Failed to get StartPageToken: %s", err)
						continue
					}
				}
				fs.Debugf(f, "Checking for changes on remote")
				startPageToken, err = f.changeNotifyRunner(ctx, notifyFunc, startPageToken)
				if err != nil {
					fs.Infof(f, "Change notify listener failure: %s", err)
				}
			}
		}
	}()
}

func (f *Fs) changeNotifyStartPageToken(ctx context.Context) (pageToken string, err error) {
	var startPageToken *drive.StartPageToken
	err = f.pacer.Call(func() (bool, error) {
		changes := f.svc.Changes.GetStartPageToken().SupportsAllDrives(true)
		startPageToken, err = changes.Context(ctx).Do()
		return f.shouldRetry(ctx, err)
	})
	if err != nil {
		return
	}
	return startPageToken.StartPageToken, nil
}

func (f *Fs) changeNotifyRunner(ctx context.Context, notifyFunc func(string, fs.EntryType), startPageToken string) (newStartPageToken string, err error) {
	pageToken := startPageToken
	for {
		var changeList *drive.ChangeList

		err = f.pacer.Call(func() (bool, error) {
			changesCall := f.svc.Changes.List(pageToken).
				Fields("nextPageToken,newStartPageToken,changes(fileId,file(name,parents,mimeType))")
			if f.opt.ListChunk > 0 {
				changesCall.PageSize(f.opt.ListChunk)
			}
			changesCall.SupportsAllDrives(true)
			changesCall.IncludeItemsFromAllDrives(true)
			// If using appDataFolder then need to add Spaces
			if f.rootFolderID == "appDataFolder" {
				changesCall.Spaces("appDataFolder")
			}
			changeList, err = changesCall.Context(ctx).Do()
			return f.shouldRetry(ctx, err)
		})
		if err != nil {
			return
		}

		type entryType struct {
			path      string
			entryType fs.EntryType
		}
		var pathsToClear []entryType
		for _, change := range changeList.Changes {
			// find the previous path
			if path, ok := f.dirCache.GetInv(change.FileId); ok {
				if change.File != nil && change.File.MimeType != driveFolderType {
					pathsToClear = append(pathsToClear, entryType{path: path, entryType: fs.EntryObject})
				} else {
					pathsToClear = append(pathsToClear, entryType{path: path, entryType: fs.EntryDirectory})
				}
			}

			// find the new path
			if change.File != nil {
				change.File.Name = f.opt.Enc.ToStandardName(change.File.Name)
				changeType := fs.EntryDirectory
				if change.File.MimeType != driveFolderType {
					changeType = fs.EntryObject
				}

				// translate the parent dir of this object
				if len(change.File.Parents) > 0 {
					for _, parent := range change.File.Parents {
						if parentPath, ok := f.dirCache.GetInv(parent); ok {
							// and append the drive file name to compute the full file name
							newPath := path.Join(parentPath, change.File.Name)
							// this will now clear the actual file too
							pathsToClear = append(pathsToClear, entryType{path: newPath, entryType: changeType})
						}
					}
				} else { // a true root object that is changed
					pathsToClear = append(pathsToClear, entryType{path: change.File.Name, entryType: changeType})
				}
			}
		}

		visitedPaths := make(map[string]struct{})
		for _, entry := range pathsToClear {
			if _, ok := visitedPaths[entry.path]; ok {
				continue
			}
			visitedPaths[entry.path] = struct{}{}
			notifyFunc(entry.path, entry.entryType)
		}

		switch {
		case changeList.NewStartPageToken != "":
			return changeList.NewStartPageToken, nil
		case changeList.NextPageToken != "":
			pageToken = changeList.NextPageToken
		default:
			return
		}
	}
}

// DirCacheFlush resets the directory cache - used in testing as an
// optional interface
func (f *Fs) DirCacheFlush() {
	f.dirCache.ResetRoot()
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.MD5)
}

/*
func (f *Fs) changeServiceAccountFile(ctx context.Context, file string) (err error) {
	fs.Debugf(nil, "Changing Service Account File from %s to %s", f.opt.ServiceAccountFile, file)
	if file == f.opt.ServiceAccountFile {
		return nil
	}
	oldSvc := f.svc
	oldv2Svc := f.v2Svc
	oldOAuthClient := f.client
	oldFile := f.opt.ServiceAccountFile
	oldCredentials := f.opt.ServiceAccountCredentials
	defer func() {
		// Undo all the changes instead of doing selective undo's
		if err != nil {
			f.svc = oldSvc
			f.v2Svc = oldv2Svc
			f.client = oldOAuthClient
			f.opt.ServiceAccountFile = oldFile
			f.opt.ServiceAccountCredentials = oldCredentials
		}
	}()
	f.opt.ServiceAccountFile = file
	f.opt.ServiceAccountCredentials = ""
	oAuthClient, err := createOAuthClient(ctx, &f.opt, f.name, f.m, f.masterKey)
	if err != nil {
		return fmt.Errorf("drive: failed when making oauth client: %w", err)
	}
	f.client = oAuthClient
	f.svc, err = drive.NewService(context.Background(), option.WithHTTPClient(f.client))
	if err != nil {
		return fmt.Errorf("couldn't create Drive client: %w", err)
	}
	if f.opt.V2DownloadMinSize >= 0 {
		f.v2Svc, err = drive_v2.NewService(context.Background(), option.WithHTTPClient(f.client))
		if err != nil {
			return fmt.Errorf("couldn't create Drive v2 client: %w", err)
		}
	}
	return nil
}*/

// Create a shortcut from (f, srcPath) to (dstFs, dstPath)
//
// Will not overwrite existing files
func (f *Fs) makeShortcut(ctx context.Context, srcPath string, dstFs *Fs, dstPath string) (o fs.Object, err error) {
	srcFs := f
	srcPath = strings.Trim(srcPath, "/")
	dstPath = strings.Trim(dstPath, "/")
	if dstPath == "" {
		return nil, errors.New("shortcut destination can't be root directory")
	}

	// Find source
	var srcID string
	isDir := false
	if srcPath == "" {
		// source is root directory
		srcID, err = f.dirCache.RootID(ctx, false)
		if err != nil {
			return nil, err
		}
		isDir = true
	} else if srcObj, err := srcFs.NewObject(ctx, srcPath); err != nil {
		if err != fs.ErrorIsDir {
			return nil, fmt.Errorf("can't find source: %w", err)
		}
		// source was a directory
		srcID, err = srcFs.dirCache.FindDir(ctx, srcPath, false)
		if err != nil {
			return nil, fmt.Errorf("failed to find source dir: %w", err)
		}
		isDir = true
	} else {
		// source was a file
		srcID = srcObj.(*Object).id
	}
	srcID = actualID(srcID) // link to underlying object not to shortcut

	// Find destination
	_, err = dstFs.NewObject(ctx, dstPath)
	if err != fs.ErrorObjectNotFound {
		if err == nil {
			err = errors.New("existing file")
		} else if err == fs.ErrorIsDir {
			err = errors.New("existing directory")
		}
		return nil, fmt.Errorf("not overwriting shortcut target: %w", err)
	}

	// Create destination shortcut
	createInfo, err := dstFs.createFileInfo(ctx, dstPath, time.Now())
	if err != nil {
		return nil, fmt.Errorf("shortcut destination failed: %w", err)
	}
	createInfo.MimeType = shortcutMimeType
	createInfo.ShortcutDetails = &drive.FileShortcutDetails{
		TargetId: srcID,
	}

	var info *drive.File
	err = dstFs.pacer.Call(func() (bool, error) {
		info, err = dstFs.svc.Files.Create(createInfo).
			Fields(partialFields).
			SupportsAllDrives(true).
			Context(ctx).Do()
		return dstFs.shouldRetry(ctx, err)
	})
	if err != nil {
		return nil, fmt.Errorf("shortcut creation failed: %w", err)
	}
	if isDir {
		return nil, nil
	}
	return dstFs.newObjectWithInfo(ctx, dstPath, info)
}

// List all team drives
func (f *Fs) listTeamDrives(ctx context.Context) (drives []*drive.Drive, err error) {
	drives = []*drive.Drive{}
	listTeamDrives := f.svc.Drives.List().PageSize(100)
	var defaultFs Fs // default Fs with default Options
	for {
		var teamDrives *drive.DriveList
		err = f.pacer.Call(func() (bool, error) {
			teamDrives, err = listTeamDrives.Context(ctx).Do()
			return defaultFs.shouldRetry(ctx, err)
		})
		if err != nil {
			return drives, fmt.Errorf("listing Team Drives failed: %w", err)
		}
		drives = append(drives, teamDrives.Drives...)
		if teamDrives.NextPageToken == "" {
			break
		}
		listTeamDrives.PageToken(teamDrives.NextPageToken)
	}
	return drives, nil
}

type unTrashResult struct {
	Untrashed int
	Errors    int
}

func (r unTrashResult) Error() string {
	return fmt.Sprintf("%d errors while untrashing - see log", r.Errors)
}

// Restore the trashed files from dir, directoryID recursing if needed
func (f *Fs) unTrash(ctx context.Context, dir string, directoryID string, recurse bool) (r unTrashResult, err error) {
	directoryID = actualID(directoryID)
	fs.Debugf(dir, "finding trash to restore in directory %q", directoryID)
	_, err = f.list(ctx, []string{directoryID}, "", false, false, f.opt.TrashedOnly, true, func(item *drive.File) bool {
		remote := path.Join(dir, item.Name)
		if item.ExplicitlyTrashed {
			fs.Infof(remote, "restoring %q", item.Id)
			if operations.SkipDestructive(ctx, remote, "restore") {
				return false
			}
			update := drive.File{
				ForceSendFields: []string{"Trashed"}, // necessary to set false value
				Trashed:         false,
			}
			err := f.pacer.Call(func() (bool, error) {
				_, err := f.svc.Files.Update(item.Id, &update).
					SupportsAllDrives(true).
					Fields("trashed").
					Context(ctx).Do()
				return f.shouldRetry(ctx, err)
			})
			if err != nil {
				err = fmt.Errorf("failed to restore: %w", err)
				r.Errors++
				fs.Errorf(remote, "%v", err)
			} else {
				r.Untrashed++
			}
		}
		if recurse && item.MimeType == "application/vnd.google-apps.folder" {
			if !isShortcutID(item.Id) {
				rNew, _ := f.unTrash(ctx, remote, item.Id, recurse)
				r.Untrashed += rNew.Untrashed
				r.Errors += rNew.Errors
			}
		}
		return false
	})
	if err != nil {
		err = fmt.Errorf("failed to list directory: %w", err)
		r.Errors++
		fs.Errorf(dir, "%v", err)
	}
	if r.Errors != 0 {
		return r, r
	}
	return r, nil
}

// Untrash dir
func (f *Fs) unTrashDir(ctx context.Context, dir string, recurse bool) (r unTrashResult, err error) {
	directoryID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		r.Errors++
		return r, err
	}
	return f.unTrash(ctx, dir, directoryID, true)
}

// copy file with id to dest
func (f *Fs) copyID(ctx context.Context, id, dest string) (err error) {
	info, err := f.getFile(ctx, id, f.fileFields)
	if err != nil {
		return fmt.Errorf("couldn't find id: %w", err)
	}
	if info.MimeType == driveFolderType {
		return fmt.Errorf("can't copy directory use: rclone copy --drive-root-folder-id %s %s %s", id, fs.ConfigString(f), dest)
	}
	info.Name = f.opt.Enc.ToStandardName(info.Name)
	o, err := f.newObjectWithInfo(ctx, info.Name, info)
	if err != nil {
		return err
	}
	destDir, destLeaf, err := fspath.Split(dest)
	if err != nil {
		return err
	}
	if destLeaf == "" {
		destLeaf = path.Base(o.Remote())
	}
	if destDir == "" {
		destDir = "."
	}
	dstFs, err := cache.Get(ctx, destDir)
	if err != nil {
		return err
	}
	_, err = operations.Copy(ctx, dstFs, nil, destLeaf, o)
	if err != nil {
		return fmt.Errorf("copy failed: %w", err)
	}
	return nil
}

// Command the backend to run a named command
//
// The command run is name
// args may be used to read arguments from
// opts may be used to read optional arguments from
//
// The result should be capable of being JSON encoded
// If it is a string or a []string it will be shown to the user
// otherwise it will be JSON encoded and shown to the user like that
func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (out interface{}, err error) {
	switch name {
	case "shortcut":
		if len(arg) != 2 {
			return nil, errors.New("need exactly 2 arguments")
		}
		dstFs := f
		target, ok := opt["target"]
		if ok {
			targetFs, err := cache.Get(ctx, target)
			if err != nil {
				return nil, fmt.Errorf("couldn't find target: %w", err)
			}
			dstFs, ok = targetFs.(*Fs)
			if !ok {
				return nil, errors.New("target is not a drive backend")
			}
		}
		return f.makeShortcut(ctx, arg[0], dstFs, arg[1])
	case "drives":
		drives, err := f.listTeamDrives(ctx)
		if err != nil {
			return nil, err
		}
		re := regexp.MustCompile(`[^\w_. -]+`)
		if _, ok := opt["config"]; ok {
			lines := []string{}
			upstreams := []string{}
			names := make(map[string]struct{}, len(drives))
			for i, drive := range drives {
				name := re.ReplaceAllString(drive.Name, "_")
				for {
					if _, found := names[name]; !found {
						break
					}
					name += fmt.Sprintf("-%d", i)
				}
				names[name] = struct{}{}
				lines = append(lines, "")
				lines = append(lines, fmt.Sprintf("[%s]", name))
				lines = append(lines, "type = alias")
				lines = append(lines, fmt.Sprintf("remote = %s,team_drive=%s,root_folder_id=:", f.name, drive.Id))
				upstreams = append(upstreams, fmt.Sprintf(`"%s=%s:"`, name, name))
			}
			lines = append(lines, "")
			lines = append(lines, "[AllDrives]")
			lines = append(lines, "type = combine")
			lines = append(lines, fmt.Sprintf("upstreams = %s", strings.Join(upstreams, " ")))
			return lines, nil
		}
		return drives, nil
	case "untrash":
		dir := ""
		if len(arg) > 0 {
			dir = arg[0]
		}
		return f.unTrashDir(ctx, dir, true)
	case "copyid":
		if len(arg)%2 != 0 {
			return nil, errors.New("need an even number of arguments")
		}
		for len(arg) > 0 {
			id, dest := arg[0], arg[1]
			arg = arg[2:]
			err = f.copyID(ctx, id, dest)
			if err != nil {
				return nil, fmt.Errorf("failed copying %q to %q: %w", id, dest, err)
			}
		}
		return nil, nil
	case "exportformats":
		return f.exportFormats(ctx), nil
	case "importformats":
		return f.importFormats(ctx), nil
	default:
		return nil, fs.ErrorCommandNotFound
	}
}

// ------------------------------------------------------------

// Fs returns the parent Fs
func (o *baseObject) Fs() fs.Info {
	return o.fs
}

// Return a string version
func (o *baseObject) String() string {
	return o.remote
}

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *baseObject) Remote() string {
	return o.remote
}

// Hash returns the Md5sum of an object returning a lowercase hex string
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	if t != hash.MD5 {
		return "", hash.ErrUnsupported
	}
	return o.md5sum, nil
}
func (o *baseObject) Hash(ctx context.Context, t hash.Type) (string, error) {
	if t != hash.MD5 {
		return "", hash.ErrUnsupported
	}
	return "", nil
}

// Size returns the size of an object in bytes
func (o *baseObject) Size() int64 {
	return o.bytes
}

// getRemoteInfoWithExport returns a drive.File and the export settings for the remote
func (f *Fs) getRemoteInfoWithExport(ctx context.Context, remote string) (
	info *drive.File, extension, exportName, exportMimeType string, isDocument bool, err error) {
	leaf, directoryID, err := f.dirCache.FindPath(ctx, remote, false)
	if err != nil {
		if err == fs.ErrorDirNotFound {
			return nil, "", "", "", false, fs.ErrorObjectNotFound
		}
		return nil, "", "", "", false, err
	}
	directoryID = actualID(directoryID)

	found, err := f.list(ctx, []string{directoryID}, leaf, false, false, f.opt.TrashedOnly, false, func(item *drive.File) bool {
		if !f.opt.SkipGdocs {
			extension, exportName, exportMimeType, isDocument = f.findExportFormat(ctx, item)
			if exportName == leaf {
				info = item
				return true
			}
			if isDocument {
				return false
			}
		}
		if item.Name == leaf {
			info = item
			return true
		}
		return false
	})
	if err != nil {
		return nil, "", "", "", false, err
	}
	if !found {
		return nil, "", "", "", false, fs.ErrorObjectNotFound
	}
	return
}

// ModTime returns the modification time of the object
//
// It attempts to read the objects mtime and if that isn't present the
// LastModified returned in the http headers
func (o *baseObject) ModTime(ctx context.Context) time.Time {
	modTime, err := time.Parse(timeFormatIn, o.modifiedDate)
	if err != nil {
		fs.Debugf(o, "Failed to read mtime from object: %v", err)
		return time.Now()
	}
	return modTime
}

// SetModTime sets the modification time of the drive fs object
func (o *baseObject) SetModTime(ctx context.Context, modTime time.Time) error {
	// New metadata
	updateInfo := &drive.File{
		ModifiedTime: modTime.Format(timeFormatOut),
	}
	// Set modified date
	var info *drive.File
	err := o.fs.pacer.Call(func() (bool, error) {
		var err error
		info, err = o.fs.svc.Files.Update(actualID(o.id), updateInfo).
			Fields(partialFields).
			SupportsAllDrives(true).
			Context(ctx).Do()
		return o.fs.shouldRetry(ctx, err)
	})
	if err != nil {
		return err
	}
	// Update info from read data
	o.modifiedDate = info.ModifiedTime
	return nil
}

// Storable returns a boolean as to whether this object is storable
func (o *baseObject) Storable() bool {
	return true
}

// addResourceKey adds a X-Goog-Drive-Resource-Keys header for this
// object if required.
func (o *baseObject) addResourceKey(header http.Header) {
	if o.resourceKey != nil {
		header.Add("X-Goog-Drive-Resource-Keys", fmt.Sprintf("%s/%s", o.id, *o.resourceKey))
	}
}

// httpResponse gets an http.Response object for the object
// using the url and method passed in
func (o *baseObject) httpResponse(ctx context.Context, url, method string, options []fs.OpenOption) (req *http.Request, res *http.Response, err error) {
	if url == "" {
		return nil, nil, errors.New("forbidden to download - check sharing permission")
	}
	req, err = http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return req, nil, err
	}
	fs.OpenOptionAddHTTPHeaders(req.Header, options)
	if o.bytes == 0 {
		// Don't supply range requests for 0 length objects as they always fail
		delete(req.Header, "Range")
	}
	o.addResourceKey(req.Header)
	err = o.fs.pacer.Call(func() (bool, error) {
		res, err = o.fs.client.Do(req)
		if err == nil {
			err = googleapi.CheckResponse(res)
			if err != nil {
				_ = res.Body.Close() // ignore error
			}
		}
		return o.fs.shouldRetry(ctx, err)
	})
	if err != nil {
		return req, nil, err
	}
	return req, res, nil
}

// openDocumentFile represents a documentObject open for reading.
// Updates the object size after read successfully.
type openDocumentFile struct {
	o       *documentObject // Object we are reading for
	in      io.ReadCloser   // reading from here
	bytes   int64           // number of bytes read on this connection
	eof     bool            // whether we have read end of file
	errored bool            // whether we have encountered an error during reading
}

// Read bytes from the object - see io.Reader
func (file *openDocumentFile) Read(p []byte) (n int, err error) {
	n, err = file.in.Read(p)
	file.bytes += int64(n)
	if err != nil && err != io.EOF {
		file.errored = true
	}
	if err == io.EOF {
		file.eof = true
	}
	return
}

// Close the object and update bytes read
func (file *openDocumentFile) Close() (err error) {
	// If end of file, update bytes read
	if file.eof && !file.errored {
		fs.Debugf(file.o, "Updating size of doc after download to %v", file.bytes)
		file.o.bytes = file.bytes
	}
	return file.in.Close()
}

// Check it satisfies the interfaces
var _ io.ReadCloser = (*openDocumentFile)(nil)

// Checks to see if err is a googleapi.Error with of type what
func isGoogleError(err error, what string) bool {
	if gerr, ok := err.(*googleapi.Error); ok {
		for _, error := range gerr.Errors {
			if error.Reason == what {
				return true
			}
		}
	}
	return false
}

// open a url for reading
func (o *baseObject) open(ctx context.Context, url string, options ...fs.OpenOption) (in io.ReadCloser, err error) {
	_, res, err := o.httpResponse(ctx, url, "GET", options)
	if err != nil {
		if isGoogleError(err, "cannotDownloadAbusiveFile") {
			if o.fs.opt.AcknowledgeAbuse {
				// Retry acknowledging abuse
				if strings.ContainsRune(url, '?') {
					url += "&"
				} else {
					url += "?"
				}
				url += "acknowledgeAbuse=true"
				_, res, err = o.httpResponse(ctx, url, "GET", options)
			} else {
				err = fmt.Errorf("use the --drive-acknowledge-abuse flag to download this file: %w", err)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("open file failed: %w", err)
		}
	}
	return res.Body, nil
}

// Open an object for read
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (in io.ReadCloser, err error) {
	if o.mimeType == shortcutMimeTypeDangling {
		return nil, errors.New("can't read dangling shortcut")
	}
	if o.v2Download {
		var v2File *drive_v2.File
		err = o.fs.pacer.Call(func() (bool, error) {
			v2File, err = o.fs.v2Svc.Files.Get(actualID(o.id)).
				Fields("downloadUrl").
				SupportsAllDrives(true).
				Context(ctx).Do()
			return o.fs.shouldRetry(ctx, err)
		})
		if err == nil {
			fs.Debugf(o, "Using v2 download: %v", v2File.DownloadUrl)
			o.url = v2File.DownloadUrl
			o.v2Download = false
		}
	}
	return o.baseObject.open(ctx, o.url, options...)
}
func (o *documentObject) Open(ctx context.Context, options ...fs.OpenOption) (in io.ReadCloser, err error) {
	// Update the size with what we are reading as it can change from
	// the HEAD in the listing to this GET. This stops rclone marking
	// the transfer as corrupted.
	var offset, end int64 = 0, -1
	var newOptions = options[:0]
	for _, o := range options {
		// Note that Range requests don't work on Google docs:
		// https://developers.google.com/drive/v3/web/manage-downloads#partial_download
		// So do a subset of them manually
		switch x := o.(type) {
		case *fs.RangeOption:
			offset, end = x.Start, x.End
		case *fs.SeekOption:
			offset, end = x.Offset, -1
		default:
			newOptions = append(newOptions, o)
		}
	}
	options = newOptions
	if offset != 0 {
		return nil, errors.New("partial downloads are not supported while exporting Google Documents")
	}
	in, err = o.baseObject.open(ctx, o.url, options...)
	if in != nil {
		in = &openDocumentFile{o: o, in: in}
	}
	if end >= 0 {
		in = readers.NewLimitedReadCloser(in, end-offset+1)
	}
	return
}
func (o *linkObject) Open(ctx context.Context, options ...fs.OpenOption) (in io.ReadCloser, err error) {
	var offset, limit int64 = 0, -1
	var data = o.content
	for _, option := range options {
		switch x := option.(type) {
		case *fs.SeekOption:
			offset = x.Offset
		case *fs.RangeOption:
			offset, limit = x.Decode(int64(len(data)))
		default:
			if option.Mandatory() {
				fs.Logf(o, "Unsupported mandatory option: %v", option)
			}
		}
	}
	if l := int64(len(data)); offset > l {
		offset = l
	}
	data = data[offset:]
	if limit != -1 && limit < int64(len(data)) {
		data = data[:limit]
	}

	return io.NopCloser(bytes.NewReader(data)), nil
}

func (o *baseObject) update(ctx context.Context, updateInfo *drive.File, uploadMimeType string, in io.Reader,
	src fs.ObjectInfo) (info *drive.File, err error) {
	// Make the API request to upload metadata and file data.
	size := src.Size()
	if size >= 0 && size < int64(o.fs.opt.UploadCutoff) {
		// Don't retry, return a retry error instead
		err = o.fs.pacer.CallNoRetry(func() (bool, error) {
			info, err = o.fs.svc.Files.Update(actualID(o.id), updateInfo).
				Media(in, googleapi.ContentType(uploadMimeType), googleapi.ChunkSize(0)).
				Fields(partialFields).
				SupportsAllDrives(true).
				Context(ctx).Do()
			return o.fs.shouldRetry(ctx, err)
		})
		return
	}
	// Upload the file in chunks
	return o.fs.Upload(ctx, in, size, uploadMimeType, o.id, o.remote, updateInfo)
}

// Update the already existing object
//
// Copy the reader into the object updating modTime and size.
//
// The new object may have been created if an error is returned
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	// If o is a shortcut
	if isShortcutID(o.id) {
		// Delete it first
		err := o.fs.delete(ctx, shortcutID(o.id), false)
		if err != nil {
			return err
		}
		// Then put the file as a new file
		newObj, err := o.fs.PutUnchecked(ctx, in, src, options...)
		if err != nil {
			return err
		}
		// Update the object
		if newO, ok := newObj.(*Object); ok {
			*o = *newO
		} else {
			fs.Debugf(newObj, "Failed to update object %T from new object %T", o, newObj)
		}
		return nil
	}
	srcMimeType := fs.MimeType(ctx, src)
	updateInfo := &drive.File{
		MimeType:     srcMimeType,
		ModifiedTime: src.ModTime(ctx).Format(timeFormatOut),
	}
	info, err := o.baseObject.update(ctx, updateInfo, srcMimeType, in, src)
	if err != nil {
		return err
	}
	newO, err := o.fs.newObjectWithInfo(ctx, src.Remote(), info)
	if err != nil {
		return err
	}
	switch newO := newO.(type) {
	case *Object:
		*o = *newO
	default:
		return errors.New("object type changed by update")
	}

	return nil
}
func (o *documentObject) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	srcMimeType := fs.MimeType(ctx, src)
	importMimeType := ""
	updateInfo := &drive.File{
		MimeType:     srcMimeType,
		ModifiedTime: src.ModTime(ctx).Format(timeFormatOut),
	}

	if o.fs.importMimeTypes == nil || o.fs.opt.SkipGdocs {
		return fmt.Errorf("can't update google document type without --drive-import-formats")
	}
	importMimeType = o.fs.findImportFormat(ctx, updateInfo.MimeType)
	if importMimeType == "" {
		return fmt.Errorf("no import format found for %q", srcMimeType)
	}
	if importMimeType != o.documentMimeType {
		return fmt.Errorf("can't change google document type (o: %q, src: %q, import: %q)", o.documentMimeType, srcMimeType, importMimeType)
	}
	updateInfo.MimeType = importMimeType

	info, err := o.baseObject.update(ctx, updateInfo, srcMimeType, in, src)
	if err != nil {
		return err
	}

	remote := src.Remote()
	remote = remote[:len(remote)-o.extLen]

	newO, err := o.fs.newObjectWithInfo(ctx, remote, info)
	if err != nil {
		return err
	}
	switch newO := newO.(type) {
	case *documentObject:
		*o = *newO
	default:
		return errors.New("object type changed by update")
	}

	return nil
}

func (o *linkObject) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	return errors.New("cannot update link files")
}

// Remove an object
func (o *baseObject) Remove(ctx context.Context) error {
	if len(o.parents) > 1 {
		return errors.New("can't delete safely - has multiple parents")
	}
	fs.Debugf("==>", "Original id: %s, resolved id: %s", o.id, shortcutID(o.id))
	return o.fs.delete(ctx, shortcutID(o.id), false)
}

// MimeType of an Object if known, "" otherwise
func (o *baseObject) MimeType(ctx context.Context) string {
	return o.mimeType
}

// ID returns the ID of the Object if known, or "" if not
func (o *baseObject) ID() string {
	return o.id
}

// ParentID returns the ID of the Object parent if known, or "" if not
func (o *baseObject) ParentID() string {
	if len(o.parents) > 0 {
		return o.parents[0]
	}
	return ""
}

func (o *documentObject) ext() string {
	return o.baseObject.remote[len(o.baseObject.remote)-o.extLen:]
}
func (o *linkObject) ext() string {
	return o.baseObject.remote[len(o.baseObject.remote)-o.extLen:]
}

// templates for document link files
const (
	urlTemplate = `[InternetShortcut]{{"\r"}}
URL={{ .URL }}{{"\r"}}
`
	weblocTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>URL</key>
    <string>{{ .URL }}</string>
  </dict>
</plist>
`
	desktopTemplate = `[Desktop Entry]
Encoding=UTF-8
Name={{ .Title }}
URL={{ .URL }}
Icon={{ .XDGIcon }}
Type=Link
`
	htmlTemplate = `<html>
<head>
  <meta http-equiv="refresh" content="0; url={{ .URL }}" />
  <title>{{ .Title }}</title>
</head>
<body>
  Loading <a href="{{ .URL }}">{{ .Title }}</a>
</body>
</html>
`
)

// Check the interfaces are satisfied
var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ fs.CleanUpper      = (*Fs)(nil)
	_ fs.PutStreamer     = (*Fs)(nil)
	_ fs.Copier          = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.Commander       = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	_ fs.ChangeNotifier  = (*Fs)(nil)
	_ fs.PutUncheckeder  = (*Fs)(nil)
	_ fs.PublicLinker    = (*Fs)(nil)
	_ fs.ListRer         = (*Fs)(nil)
	_ fs.MergeDirser     = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
	_ fs.MimeTyper       = (*Object)(nil)
	_ fs.IDer            = (*Object)(nil)
	_ fs.ParentIDer      = (*Object)(nil)
	_ fs.Object          = (*documentObject)(nil)
	_ fs.MimeTyper       = (*documentObject)(nil)
	_ fs.IDer            = (*documentObject)(nil)
	_ fs.ParentIDer      = (*documentObject)(nil)
	_ fs.Object          = (*linkObject)(nil)
	_ fs.MimeTyper       = (*linkObject)(nil)
	_ fs.IDer            = (*linkObject)(nil)
	_ fs.ParentIDer      = (*linkObject)(nil)
)

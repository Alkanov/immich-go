// Command Upload

package cmdupload

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/simulot/immich-go/browser"
	"github.com/simulot/immich-go/browser/files"
	"github.com/simulot/immich-go/browser/gp"
	"github.com/simulot/immich-go/helpers/fshelper"
	"github.com/simulot/immich-go/helpers/fshelper/myflag"
	"github.com/simulot/immich-go/helpers/gen"
	"github.com/simulot/immich-go/helpers/stacking"
	"github.com/simulot/immich-go/immich"
	"github.com/simulot/immich-go/immich/metadata"

	"github.com/simulot/immich-go/logger"
)

// iClient is an interface that implements the minimal immich client set of features for uploading
// interface used to mock up the client
type iClient interface {
	GetAllAssetsWithFilter(context.Context, *immich.GetAssetOptions, func(*immich.Asset)) error
	AssetUpload(context.Context, *browser.LocalAssetFile) (immich.AssetResponse, error)
	DeleteAssets(context.Context, []string, bool) error

	GetAllAlbums(context.Context) ([]immich.AlbumSimplified, error)
	AddAssetToAlbum(context.Context, string, []string) ([]immich.UpdateAlbumResult, error)
	CreateAlbum(context.Context, string, []string) (immich.AlbumSimplified, error)
	UpdateAssets(ctx context.Context, IDs []string, isArchived bool, isFavorite bool, latitude float64, longitude float64, removeParent bool, stackParentId string) error
	StackAssets(ctx context.Context, cover string, IDs []string) error
	UpdateAsset(ctx context.Context, ID string, a *browser.LocalAssetFile) (*immich.Asset, error)
}

type UpCmd struct {
	client  iClient         // Immich client
	Journal *logger.Journal // Log and journal

	fsys []fs.FS // pseudo file system to browse

	GooglePhotos           bool             // For reading Google Photos takeout files
	Delete                 bool             // Delete original file after import
	CreateAlbumAfterFolder bool             // Create albums for assets based on the parent folder or a given name
	ImportIntoAlbum        string           // All assets will be added to this album
	PartnerAlbum           string           // Partner's assets will be added to this album
	Import                 bool             // Import instead of upload
	DeviceUUID             string           // Set a device UUID
	Paths                  []string         // Path to explore
	DateRange              immich.DateRange // Set capture date range
	ImportFromAlbum        string           // Import assets from this albums
	CreateAlbums           bool             // Create albums when exists in the source
	KeepTrashed            bool             // Import trashed assets
	KeepPartner            bool             // Import partner's assets
	KeepUntitled           bool             // Keep untitled albums
	UseFolderAsAlbumName   bool             // Use folder's name instead of metadata's title as Album name
	DryRun                 bool             // Display actions but don't change anything
	ForceSidecar           bool             // Generate a sidecar file for each file (default: TRUE)
	CreateStacks           bool             // Stack jpg/raw/burst (Default: TRUE)
	StackJpgRaws           bool             // Stack jpg/raw (Default: TRUE)
	StackBurst             bool             // Stack burst (Default: TRUE)
	DiscardArchived        bool             // Don't import archived assets (Default: FALSE)

	BrowserConfig Configuration

	AssetIndex       *AssetIndex               // List of assets present on the server
	deleteServerList []*immich.Asset           // List of server assets to remove
	deleteLocalList  []*browser.LocalAssetFile // List of local assets to remove
	mediaUploaded    int                       // Count uploaded medias
	mediaCount       int                       // Count of media on the source
	updateAlbums     map[string]map[string]any // track immich albums changes
	stacks           *stacking.StackBuilder
}

func NewUpCmd(ctx context.Context, ic iClient, log logger.Logger, args []string) (*UpCmd, error) {
	var err error
	cmd := flag.NewFlagSet("upload", flag.ExitOnError)

	app := UpCmd{
		updateAlbums: map[string]map[string]any{},
		Journal:      logger.NewJournal(log),
		client:       ic,
	}
	cmd.BoolFunc(
		"dry-run",
		"display actions but don't touch source or destination",
		myflag.BoolFlagFn(&app.DryRun, false))
	cmd.Var(&app.DateRange,
		"date",
		"Date of capture range.")
	cmd.StringVar(&app.ImportIntoAlbum,
		"album",
		"",
		"All assets will be added to this album.")
	cmd.BoolFunc(
		"force-sidecar",
		"Upload the photo and a sidecar file with known information like date and GPS coordinates. With google-photos, information comes from the metadata files. (DEFAULT false)",
		myflag.BoolFlagFn(&app.ForceSidecar, false))
	cmd.BoolFunc(
		"create-album-folder",
		" folder import only: Create albums for assets based on the parent folder",
		myflag.BoolFlagFn(&app.CreateAlbumAfterFolder, false))
	cmd.BoolFunc(
		"google-photos",
		"Import GooglePhotos takeout zip files",
		myflag.BoolFlagFn(&app.GooglePhotos, false))
	cmd.BoolFunc(
		"create-albums",
		" google-photos only: Create albums like there were in the source (default: TRUE)",
		myflag.BoolFlagFn(&app.CreateAlbums, true))
	cmd.StringVar(&app.PartnerAlbum,
		"partner-album",
		"",
		" google-photos only: Assets from partner will be added to this album. (ImportIntoAlbum, must already exist)")
	cmd.BoolFunc(
		"keep-partner",
		" google-photos only: Import also partner's items (default: TRUE)", myflag.BoolFlagFn(&app.KeepPartner, true))
	cmd.StringVar(&app.ImportFromAlbum,
		"from-album",
		"",
		" google-photos only: Import only from this album")

	cmd.BoolFunc(
		"keep-untitled-albums",
		" google-photos only: Keep Untitled albums and imports their contain (default: FALSE)", myflag.BoolFlagFn(&app.KeepUntitled, false))

	cmd.BoolFunc(
		"use-album-folder-as-name",
		" google-photos only: Use folder name and ignore albums' title (default:FALSE)", myflag.BoolFlagFn(&app.UseFolderAsAlbumName, false))

	cmd.BoolFunc(
		"discard-archived",
		" google-photos only: Do not import archived photos (default FALSE)", myflag.BoolFlagFn(&app.DiscardArchived, false))

	cmd.BoolFunc(
		"create-stacks",
		"Stack jpg/raw or bursts  (default TRUE)", myflag.BoolFlagFn(&app.CreateStacks, true))

	cmd.BoolFunc(
		"stack-jpg-raw",
		"Control the stacking of jpg/raw photos (default TRUE)", myflag.BoolFlagFn(&app.StackJpgRaws, true))
	cmd.BoolFunc(
		"stack-burst",
		"Control the stacking bursts (default TRUE)", myflag.BoolFlagFn(&app.StackBurst, true))

	// cmd.BoolVar(&app.Delete, "delete", false, "Delete local assets after upload")

	cmd.Var(&app.BrowserConfig.SelectExtensions, "select-types", "list of selected extensions separated by a comma")
	cmd.Var(&app.BrowserConfig.ExcludeExtensions, "exclude-types", "list of excluded extensions separated by a comma")

	err = cmd.Parse(args)
	if err != nil {
		return nil, err
	}

	if err = app.BrowserConfig.IsValid(); err != nil {
		return nil, err
	}

	app.Journal = logger.NewJournal(log)

	app.fsys, err = fshelper.ParsePath(cmd.Args(), app.GooglePhotos)
	if err != nil {
		return nil, err
	}

	if app.StackBurst || app.StackJpgRaws {
		app.CreateStacks = true
	}

	if app.CreateStacks || app.StackBurst || app.StackJpgRaws {
		app.stacks = stacking.NewStackBuilder()
	}
	log.OK("Ask for server's assets...")
	var list []*immich.Asset
	err = app.client.GetAllAssetsWithFilter(ctx, nil, func(a *immich.Asset) {
		if a.IsTrashed {
			return
		}
		list = append(list, a)
	})
	if err != nil {
		return nil, err
	}
	log.OK("%d asset(s) received", len(list))

	app.AssetIndex = &AssetIndex{
		assets: list,
	}

	app.AssetIndex.ReIndex()

	return &app, err

}

func UploadCommand(ctx context.Context, ic iClient, log logger.Logger, args []string) error {
	app, err := NewUpCmd(ctx, ic, log, args)
	if err != nil {
		return err
	}
	return app.Run(ctx, app.fsys)

}

func (app *UpCmd) journalAsset(a *browser.LocalAssetFile, action logger.Action, comment ...string) {
	app.Journal.AddEntry(a.FileName, action, comment...)
}

func (app *UpCmd) Run(ctx context.Context, fsyss []fs.FS) error {

	var browser browser.Browser
	var err error

	switch {
	case app.GooglePhotos:
		app.Journal.Message(logger.OK, "Browsing google take out archive...")
		browser, err = app.ReadGoogleTakeOut(ctx, fsyss)
	default:
		app.Journal.Message(logger.OK, "Browsing folder(s)...")
		browser, err = app.ExploreLocalFolder(ctx, fsyss)
	}

	if err != nil {
		app.Journal.Message(logger.Error, err.Error())
		return err
	}
	app.Journal.Message(logger.OK, "Done.")

	assetChan := browser.Browse(ctx)
assetLoop:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case a, ok := <-assetChan:
			if !ok {
				break assetLoop
			}
			if a.Err != nil {
				app.journalAsset(a, logger.ERROR, a.Err.Error())
			} else {
				err = app.handleAsset(ctx, a)
				if err != nil {
					app.journalAsset(a, logger.ERROR, err.Error())
				}
			}
		}
	}

	if app.CreateStacks {
		stacks := app.stacks.Stacks()
		if len(stacks) > 0 {
			app.Journal.OK("Creating stacks")
		nextStack:
			for _, s := range stacks {
				switch {
				case !app.StackBurst && s.StackType == stacking.StackBurst:
					continue nextStack
				case !app.StackJpgRaws && s.StackType == stacking.StackRawJpg:
					continue nextStack
				}
				app.Journal.OK("  Stacking %s...", strings.Join(s.Names, ", "))
				if !app.DryRun {
					err = app.client.StackAssets(ctx, s.CoverID, s.IDs)
					if err != nil {
						app.Journal.Warning("Can't stack images: %s", err)
					}
				}
			}
		}
	}

	if app.CreateAlbums || app.CreateAlbumAfterFolder || (app.KeepPartner && len(app.PartnerAlbum) > 0) || len(app.ImportIntoAlbum) > 0 {
		app.Journal.OK("Managing albums")
		err = app.ManageAlbums(ctx)
		if err != nil {
			app.Journal.Error(err.Error())
			err = nil
		}
	}

	if len(app.deleteServerList) > 0 {
		ids := []string{}
		for _, da := range app.deleteServerList {
			ids = append(ids, da.ID)
		}
		err := app.DeleteServerAssets(ctx, ids)
		if err != nil {
			return fmt.Errorf("can't delete server's assets: %w", err)
		}
	}

	if len(app.deleteLocalList) > 0 {
		err = app.DeleteLocalAssets()
	}

	app.Journal.Report()

	return err
}

func (app *UpCmd) handleAsset(ctx context.Context, a *browser.LocalAssetFile) error {
	defer func() {
		a.Close()
	}()
	app.mediaCount++

	// ext := path.Ext(a.FileName)
	// if _, err := fshelper.MimeFromExt(ext); err != nil {
	// 	app.journalAsset(a, logger.NOT_SELECTED, "not recognized extension")
	// 	return nil
	// }

	if !app.KeepPartner && a.FromPartner {
		app.journalAsset(a, logger.NOT_SELECTED, "partners asset excluded")
		return nil
	}

	if !app.KeepTrashed && a.Trashed {
		app.journalAsset(a, logger.NOT_SELECTED, "trashed asset excluded")
		return nil
	}

	if len(app.ImportFromAlbum) > 0 && !app.isInAlbum(a, app.ImportFromAlbum) {
		app.journalAsset(a, logger.NOT_SELECTED, "asset excluded because not from the required album")
		return nil
	}

	if app.DiscardArchived && a.Archived {
		app.journalAsset(a, logger.NOT_SELECTED, "asset excluded because archives are discarded")
		return nil
	}

	if app.DateRange.IsSet() {
		d := a.DateTaken
		if d.IsZero() {
			app.journalAsset(a, logger.NOT_SELECTED, "asset excluded because the date of capture is unknown and a date range is given")
			return nil
		}
		if !app.DateRange.InRange(d) {
			app.journalAsset(a, logger.NOT_SELECTED, "asset excluded because the date of capture out of the date range")
			return nil
		}
	}

	if !app.KeepUntitled {
		a.Albums = gen.Filter(a.Albums, func(i browser.LocalAlbum) bool {
			return i.Name != ""
		})
	}

	app.Journal.DebugObject("handleAsset: LocalAssetFile=", a)

	advice, err := app.AssetIndex.ShouldUpload(a)
	if err != nil {
		return err
	}

	var ID string
	switch advice.Advice {
	case NotOnServer:
		ID, err = app.UploadAsset(ctx, a)
		if app.Delete && err == nil {
			app.deleteLocalList = append(app.deleteLocalList, a)
		}
	case SmallerOnServer:
		app.journalAsset(a, logger.UPGRADED, advice.Message)
		// add the superior asset into albums of the original asset
		for _, al := range advice.ServerAsset.Albums {
			app.journalAsset(a, logger.INFO, "Added to album: "+al.AlbumName)
			a.AddAlbum(browser.LocalAlbum{Name: al.AlbumName})
		}
		ID, err = app.UploadAsset(ctx, a)

		if err != nil {
			app.deleteServerList = append(app.deleteServerList, advice.ServerAsset)
			if app.Delete {
				app.deleteLocalList = append(app.deleteLocalList, a)
			}
		}
	case SameOnServer:
		// Set add the server asset into albums determined locally
		if !advice.ServerAsset.JustUploaded {
			app.journalAsset(a, logger.SERVER_DUPLICATE, advice.Message)
		} else {
			app.journalAsset(a, logger.LOCAL_DUPLICATE)
		}
		ID = advice.ServerAsset.ID
		if app.CreateAlbums {
			for _, al := range a.Albums {
				app.journalAsset(a, logger.INFO, "Added to album: "+al.Name)
				app.AddToAlbum(advice.ServerAsset.ID, app.albumName(al))
			}
		}
		if app.ImportIntoAlbum != "" {
			app.journalAsset(a, logger.INFO, "Added to album: "+app.ImportIntoAlbum)
			app.AddToAlbum(advice.ServerAsset.ID, app.ImportIntoAlbum)
		}
		if app.PartnerAlbum != "" && a.FromPartner {
			app.journalAsset(a, logger.INFO, "Added to album: "+app.PartnerAlbum)
			app.AddToAlbum(advice.ServerAsset.ID, app.PartnerAlbum)
		}
		if !advice.ServerAsset.JustUploaded {
			if app.Delete {
				app.deleteLocalList = append(app.deleteLocalList, a)
			}
		} else {
			return nil
		}
	case BetterOnServer:
		app.journalAsset(a, logger.SERVER_BETTER, advice.Message)
		ID = advice.ServerAsset.ID
		// keep the server version but update albums
		if app.CreateAlbums {
			for _, al := range a.Albums {
				app.journalAsset(a, logger.INFO, "Added to album: "+al.Name)
				app.AddToAlbum(advice.ServerAsset.ID, app.albumName(al))
			}
		}
		if app.PartnerAlbum != "" && a.FromPartner {
			app.journalAsset(a, logger.INFO, "Added to album: "+app.PartnerAlbum)
			app.AddToAlbum(advice.ServerAsset.ID, app.PartnerAlbum)
		}
	}

	if err != nil {
		return nil
	}

	if app.ImportIntoAlbum != "" ||
		(app.GooglePhotos && (app.CreateAlbums || app.PartnerAlbum != "")) ||
		(!app.GooglePhotos && app.CreateAlbumAfterFolder) {
		albums := []browser.LocalAlbum{}

		if app.ImportIntoAlbum != "" {
			albums = append(albums, browser.LocalAlbum{Path: app.ImportIntoAlbum, Name: app.ImportIntoAlbum})
		} else {
			switch {
			case app.GooglePhotos:
				albums = append(albums, a.Albums...)
				if app.PartnerAlbum != "" && a.FromPartner {
					albums = append(albums, browser.LocalAlbum{Path: app.PartnerAlbum, Name: app.PartnerAlbum})
				}
			case !app.GooglePhotos && app.CreateAlbumAfterFolder:
				album := path.Base(path.Dir(a.FileName))
				if album != "" && album != "." {
					albums = append(albums, browser.LocalAlbum{Path: album, Name: album})
				}
			}
		}

		if len(albums) > 0 {
			Names := []string{}
			for _, al := range albums {
				Name := app.albumName(al)
				app.Journal.DebugObject("Add asset to the album:", al)

				if app.GooglePhotos && Name == "" {
					continue
				}
				Names = append(Names, Name)
			}
			if len(Names) > 0 {
				app.journalAsset(a, logger.ALBUM, strings.Join(Names, ", "))
				for _, n := range Names {
					app.AddToAlbum(ID, n)
				}
			}
		}
	}

	shouldUpdate := len(a.Description) > 0
	shouldUpdate = shouldUpdate || a.Favorite
	shouldUpdate = shouldUpdate || a.Longitude != 0 || a.Latitude != 0
	shouldUpdate = shouldUpdate || !a.DateTaken.IsZero()
	shouldUpdate = shouldUpdate || a.Archived

	if !app.DryRun && shouldUpdate {
		_, err := app.client.UpdateAsset(ctx, ID, a)
		if err != nil {
			app.Journal.Error("can't update the asset '%s': ", err)
		}
	}

	return nil

}

func (app *UpCmd) isInAlbum(a *browser.LocalAssetFile, album string) bool {
	for _, al := range a.Albums {
		if app.albumName(al) == album {
			return true
		}
	}
	return false
}

func (a *UpCmd) ReadGoogleTakeOut(ctx context.Context, fsyss []fs.FS) (browser.Browser, error) {
	a.Delete = false
	return gp.NewTakeout(ctx, a.Journal, fsyss...)
}

func (a *UpCmd) ExploreLocalFolder(ctx context.Context, fsyss []fs.FS) (browser.Browser, error) {
	return files.NewLocalFiles(ctx, a.Journal, fsyss...)
}

// UploadAsset upload the asset on the server
// Add the assets into listed albums
// return ID of the asset

func (app *UpCmd) UploadAsset(ctx context.Context, a *browser.LocalAssetFile) (string, error) {
	var resp immich.AssetResponse
	var err error
	if !app.DryRun {

		if app.ForceSidecar {
			sc := metadata.SideCar{}
			sc.DateTaken = a.DateTaken
			sc.Latitude = a.Latitude
			sc.Longitude = a.Longitude
			sc.Elevation = a.Altitude
			sc.FileName = a.FileName + ".xmp"
			a.SideCar = &sc
		}

		resp, err = app.client.AssetUpload(ctx, a)
	} else {
		resp.ID = uuid.NewString()
	}
	if err != nil {
		app.journalAsset(a, logger.SERVER_ERROR, err.Error())
		return "", err
	}
	if !resp.Duplicate {
		app.journalAsset(a, logger.UPLOADED, a.Title)
		app.AssetIndex.AddLocalAsset(a, resp.ID)
		app.mediaUploaded += 1
		if app.CreateStacks {
			app.stacks.ProcessAsset(resp.ID, a.FileName, a.DateTaken)
		}

	} else {
		app.journalAsset(a, logger.SERVER_DUPLICATE, "already on the server")
	}

	return resp.ID, nil
}

func (app *UpCmd) albumName(al browser.LocalAlbum) string {
	Name := al.Name
	if app.GooglePhotos {
		switch {
		case app.UseFolderAsAlbumName:
			Name = path.Base(al.Path)
		case app.KeepUntitled && Name == "":
			Name = path.Base(al.Path)
		}
	}
	return Name
}

func (app *UpCmd) AddToAlbum(ID string, album string) {
	l := app.updateAlbums[album]
	if l == nil {
		l = map[string]any{}
	}
	l[ID] = nil
	app.updateAlbums[album] = l
}

func (app *UpCmd) DeleteLocalAssets() error {
	app.Journal.OK("%d local assets to delete.", len(app.deleteLocalList))

	for _, a := range app.deleteLocalList {
		if !app.DryRun {
			app.Journal.Warning("delete file %q", a.Title)
			err := a.Remove()
			if err != nil {
				return err
			}
		} else {
			app.Journal.Warning("file %q not deleted, dry run mode", a.Title)
		}

	}
	return nil
}

func (app *UpCmd) DeleteServerAssets(ctx context.Context, ids []string) error {
	app.Journal.Warning("%d server assets to delete.", len(ids))

	if !app.DryRun {
		err := app.client.DeleteAssets(ctx, ids, false)
		return err
	}
	app.Journal.Warning("%d server assets to delete. skipped dry-run mode", len(ids))
	return nil
}

func (app *UpCmd) ManageAlbums(ctx context.Context) error {
	if len(app.updateAlbums) > 0 {
		serverAlbums, err := app.client.GetAllAlbums(ctx)
		if err != nil {
			return fmt.Errorf("can't get the album list from the server: %w", err)
		}
		for album, list := range app.updateAlbums {

			found := false
			for _, sal := range serverAlbums {
				if sal.AlbumName == album {
					found = true
					if !app.DryRun {
						app.Journal.OK("Update the album %s", album)
						rr, err := app.client.AddAssetToAlbum(ctx, sal.ID, gen.MapKeys(list))
						if err != nil {
							return fmt.Errorf("can't update the album list from the server: %w", err)
						}
						added := 0
						for _, r := range rr {
							if r.Success {
								added++
							}
							if !r.Success && r.Error != "duplicate" {
								app.Journal.Warning("%s: %s", r.ID, r.Error)
							}
						}
						if added > 0 {
							app.Journal.OK("%d asset(s) added to the album %q", added, album)
						}
					} else {
						app.Journal.OK("Update album %s skipped - dry run mode", album)
					}
				}
			}
			if found {
				continue
			}
			if list != nil {
				if !app.DryRun {
					app.Journal.OK("Create the album %s", album)

					_, err := app.client.CreateAlbum(ctx, album, gen.MapKeys(list))
					if err != nil {
						return fmt.Errorf("can't create the album list from the server: %w", err)
					}
				} else {
					app.Journal.OK("Create the album %s skipped - dry run mode", album)
				}
			}
		}
	}
	return nil
}

// - - go:generate stringer -type=AdviceCode
type AdviceCode int

func (a AdviceCode) String() string {
	switch a {
	case IDontKnow:
		return "IDontKnow"
	// case SameNameOnServerButNotSure:
	// 	return "SameNameOnServerButNotSure"
	case SmallerOnServer:
		return "SmallerOnServer"
	case BetterOnServer:
		return "BetterOnServer"
	case SameOnServer:
		return "SameOnServer"
	case NotOnServer:
		return "NotOnServer"
	}
	return fmt.Sprintf("advice(%d)", a)
}

const (
	IDontKnow AdviceCode = iota
	SmallerOnServer
	BetterOnServer
	SameOnServer
	NotOnServer
)

type Advice struct {
	Advice      AdviceCode
	Message     string
	ServerAsset *immich.Asset
	LocalAsset  *browser.LocalAssetFile
}

func formatBytes(s int) string {
	suffixes := []string{"B", "KB", "MB", "GB"}
	bytes := float64(s)
	base := 1024.0
	if bytes < base {
		return fmt.Sprintf("%.0f %s", bytes, suffixes[0])
	}
	exp := int64(0)
	for bytes >= base && exp < int64(len(suffixes)-1) {
		bytes /= base
		exp++
	}
	roundedSize := math.Round(bytes*10) / 10
	return fmt.Sprintf("%.1f %s", roundedSize, suffixes[exp])
}

func (ai *AssetIndex) adviceIDontKnow(la *browser.LocalAssetFile) *Advice {
	return &Advice{
		Advice:     IDontKnow,
		Message:    fmt.Sprintf("Can't decide what to do with %q. Check this file", la.FileName),
		LocalAsset: la,
	}
}

func (ai *AssetIndex) adviceSameOnServer(sa *immich.Asset) *Advice {

	return &Advice{
		Advice:      SameOnServer,
		Message:     fmt.Sprintf("An asset with the same name:%q, date:%q and size:%s exists on the server. No need to upload.", sa.OriginalFileName, sa.ExifInfo.DateTimeOriginal.Format(time.DateTime), formatBytes(sa.ExifInfo.FileSizeInByte)),
		ServerAsset: sa,
	}
}
func (ai *AssetIndex) adviceSmallerOnServer(sa *immich.Asset) *Advice {
	return &Advice{
		Advice:      SmallerOnServer,
		Message:     fmt.Sprintf("An asset with the same name:%q and date:%q but with smaller size:%s exists on the server. Replace it.", sa.OriginalFileName, sa.ExifInfo.DateTimeOriginal.Format(time.DateTime), formatBytes(sa.ExifInfo.FileSizeInByte)),
		ServerAsset: sa,
	}
}
func (ai *AssetIndex) adviceBetterOnServer(sa *immich.Asset) *Advice {
	return &Advice{
		Advice:      BetterOnServer,
		Message:     fmt.Sprintf("An asset with the same name:%q and date:%q but with bigger size:%s exists on the server. No need to upload.", sa.OriginalFileName, sa.ExifInfo.DateTimeOriginal.Format(time.DateTime), formatBytes(sa.ExifInfo.FileSizeInByte)),
		ServerAsset: sa,
	}
}
func (ai *AssetIndex) adviceNotOnServer() *Advice {
	return &Advice{
		Advice:  NotOnServer,
		Message: "This a new asset, upload it.",
	}
}

// ShouldUpload check if the server has this asset
//
// The server may have different assets with the same name. This happens with photos produced by digital cameras.
// The server may have the asset, but in lower resolution. Compare the taken date and resolution

func (ai *AssetIndex) ShouldUpload(la *browser.LocalAssetFile) (*Advice, error) {
	filename := la.Title
	if path.Ext(filename) == "" {
		filename += path.Ext(la.FileName)
	}
	var err error
	ID := la.DeviceAssetID()

	sa := ai.byID[ID]
	if sa != nil {
		// the same ID exist on the server
		return ai.adviceSameOnServer(sa), nil
	}

	var l []*immich.Asset

	// check all files with the same name

	n := filepath.Base(filename)
	l = ai.byName[n]
	if len(l) == 0 {
		// n = strings.TrimSuffix(n, filepath.Ext(n))
		l = ai.byName[n]
	}

	if len(l) > 0 {
		dateTaken := la.DateTaken
		size := int(la.Size())
		if err != nil {
			return ai.adviceIDontKnow(la), nil

		}
		for _, sa = range l {
			compareDate := compareDate(dateTaken, sa.ExifInfo.DateTimeOriginal.Time)
			compareSize := size - sa.ExifInfo.FileSizeInByte

			switch {
			case compareDate == 0 && compareSize == 0:
				return ai.adviceSameOnServer(sa), nil
			case compareDate == 0 && compareSize > 0:
				return ai.adviceSmallerOnServer(sa), nil
			case compareDate == 0 && compareSize < 0:
				return ai.adviceBetterOnServer(sa), nil
			}
		}
	}
	return ai.adviceNotOnServer(), nil
}

func compareDate(d1 time.Time, d2 time.Time) int {
	diff := d1.Sub(d2)

	switch {
	case diff < -5*time.Minute:
		return -1
	case diff >= 5*time.Minute:
		return +1
	}
	return 0
}

package folder

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/simulot/immich-go/adapters"
	"github.com/simulot/immich-go/app"
	"github.com/simulot/immich-go/internal/assets"
	"github.com/simulot/immich-go/internal/exif"
	"github.com/simulot/immich-go/internal/exif/sidecars/jsonsidecar"
	"github.com/simulot/immich-go/internal/exif/sidecars/xmpsidecar"
	"github.com/simulot/immich-go/internal/fileevent"
	"github.com/simulot/immich-go/internal/filenames"
	"github.com/simulot/immich-go/internal/filetypes"
	"github.com/simulot/immich-go/internal/filters"
	"github.com/simulot/immich-go/internal/fshelper"
	"github.com/simulot/immich-go/internal/gen"
	"github.com/simulot/immich-go/internal/groups"
	"github.com/simulot/immich-go/internal/groups/burst"
	"github.com/simulot/immich-go/internal/groups/epsonfastfoto"
	"github.com/simulot/immich-go/internal/groups/series"
	"github.com/simulot/immich-go/internal/namematcher"
	"github.com/simulot/immich-go/internal/worker"
	"github.com/spf13/cobra"
)

func (ifc *ImportFolderCmd) run(cmd *cobra.Command, args []string, app *app.Application, runner adapters.Runner) error {
	var err error
	if ifc.ImportIntoAlbum != "" && ifc.UsePathAsAlbumName != FolderModeNone {
		return errors.New("cannot use both --into-album and --folder-as-album flags")
	}

	ifc.app = app
	ifc.processor = app.FileProcessor()
	ifc.tz = app.GetTZ()
	// ifc.InclusionFlags.SetIncludeTypeExtensions()

	// parse arguments and generate a fs.FS per argument
	ifc.fsyss, err = fshelper.ParsePath(args)
	if err != nil {
		return err
	}
	if len(ifc.fsyss) == 0 {
		app.Log().Message("No file found matching the pattern: %s", strings.Join(args, ","))
		return errors.New("No file found matching the pattern: " + strings.Join(args, ","))
	}

	defer func() {
		if err := fshelper.CloseFSs(ifc.fsyss); err != nil {
			// Handle the error - log it, since we can't return it
			app.Log().Error("error closing file systems", "error", err)
		}
	}()

	// Start the workers
	ifc.pool = worker.NewPool(ifc.app.ConcurrentTask)
	defer ifc.pool.Stop()
	// create the adapter for folders
	ifc.supportedMedia = ifc.app.GetSupportedMedia()

	if f := cmd.Flags().Lookup("manage-burst"); f != nil {
		_ = ifc.ManageBurst.Set(f.Value.String())
	}
	if f := cmd.Flags().Lookup("manage-heic-jpeg"); f != nil {
		_ = ifc.ManageHEICJPG.Set(f.Value.String())
	}
	if f := cmd.Flags().Lookup("manage-raw-jpeg"); f != nil {
		_ = ifc.ManageRawJPG.Set(f.Value.String())
	}
	if b, err := cmd.Flags().GetBool("manage-epson-fastfoto"); err == nil {
		ifc.ManageEpsonFastFoto = b
	}

	ifc.isArchive = cmd.Parent() != nil && cmd.Parent().Name() == "archive"
	ifc.requiresDateInformation = ifc.InclusionFlags.DateRange.IsSet() ||
		ifc.TakeDateFromFilename || ifc.ManageBurst != filters.BurstNothing ||
		ifc.ManageHEICJPG != filters.HeicJpgNothing || ifc.ManageRawJPG != filters.RawJPGNothing ||
		ifc.isArchive
	if ifc.PicasaAlbum {
		ifc.picasaAlbums = gen.NewSyncMap[string, PicasaAlbum]() // make(map[string]PicasaAlbum)
	}
	if ifc.ICloudTakeout {
		ifc.icloudMetas = gen.NewSyncMap[string, iCloudMeta]()
		ifc.icloudMetaPass = true
	}

	if ifc.infoCollector == nil {
		ifc.infoCollector = filenames.NewInfoCollector(ifc.tz, ifc.supportedMedia)
	}

	if ifc.InclusionFlags.DateRange.IsSet() {
		ifc.InclusionFlags.DateRange.SetTZ(ifc.tz)
	}

	if ifc.ManageEpsonFastFoto {
		ifc.groupers = append(ifc.groupers, epsonfastfoto.Group{}.Group)
	}
	if ifc.ManageBurst != filters.BurstNothing {
		ifc.groupers = append(ifc.groupers, burst.Group)
	}
	ifc.groupers = append(ifc.groupers, series.Group)

	// callback the caller
	err = runner.Run(cmd, ifc)
	return err
}

const icloudMetadataExt = ".csv"

func (ifc *ImportFolderCmd) Browse(ctx context.Context) chan *assets.Group {
	gOut := make(chan *assets.Group, max(ifc.app.ConcurrentTask, 4))
	go func() {
		defer func() {
			close(gOut)
		}()
		// two passes for icloud takouts
		if ifc.icloudMetaPass {
			for _, fsys := range ifc.fsyss {
				ifc.concurrentParseDir(ctx, fsys, ".", gOut)
			}
			ifc.wg.Wait()
			ifc.icloudMetaPass = false
		}
		for _, fsys := range ifc.fsyss {
			ifc.concurrentParseDir(ctx, fsys, ".", gOut)
		}
		ifc.wg.Wait()
		ifc.pool.Stop()
	}()
	return gOut
}

func (ifc *ImportFolderCmd) concurrentParseDir(ctx context.Context, fsys fs.FS, dir string, gOut chan *assets.Group) {
	ifc.wg.Add(1)
	go ifc.pool.Submit(func() {
		defer ifc.wg.Done()
		err := ifc.parseDir(ctx, fsys, dir, gOut)
		if err != nil && !errors.Is(err, context.Canceled) {
			ifc.app.Log().Error(err.Error())
		}
	})
}

func (ifc *ImportFolderCmd) parseDir(ctx context.Context, fsys fs.FS, dir string, gOut chan *assets.Group) error {
	fsName := ""
	if fsys, ok := fsys.(interface{ Name() string }); ok {
		fsName = fsys.Name()
	}

	var as []*assets.Asset
	var entries []fs.DirEntry
	var err error

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		entries, err = fs.ReadDir(fsys, dir)
		if err != nil {
			ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, dir), 0, fileevent.ErrorFileAccess, "error", err.Error())
			return ifc.app.ProcessError(err)
		}
	}

	dirSidecars := make(map[string]string)
	for _, entry := range entries {
		if !entry.IsDir() {
			base := entry.Name()
			ext := filepath.Ext(base)
			key := strings.TrimSuffix(base, ext) + strings.ToLower(ext)
			if _, exists := dirSidecars[key]; !exists {
				dirSidecars[key] = path.Join(dir, base)
			}
		}
	}

	for _, entry := range entries {
		base := entry.Name()
		name := path.Join(dir, base)
		ext := filepath.Ext(base)

		if entry.IsDir() {
			continue
		}

		// process csv files on icloud meta pass
		if ifc.icloudMetaPass && ext == icloudMetadataExt {
			if strings.HasSuffix(strings.ToLower(dir), "albums") {
				a, err := UseICloudAlbum(ifc.icloudMetas, fsys, name)
				if err != nil {
					ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.ErrorFileAccess, "error", err.Error())
				} else {
					ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.DiscoveredMetadata, "album", a)
				}
				continue
			}
			if ifc.ICloudMemoriesAsAlbums && strings.HasSuffix(strings.ToLower(dir), "memories") {
				a, err := UseICloudMemory(ifc.icloudMetas, fsys, name)
				if err != nil {
					ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.ErrorFileAccess, "error", err.Error())
				} else {
					ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.DiscoveredMetadata, "album", a)
				}
				continue
			}
			// iCloud photo details (csv). File name pattern: "Photo Details.csv"
			if strings.HasPrefix(strings.ToLower(base), "photo details") {
				err := UseICloudPhotoDetails(ifc.icloudMetas, fsys, name)
				if err != nil {
					ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.ErrorFileAccess, "error", err.Error())
				} else {
					ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.DiscoveredMetadata)
				}
				continue
			}
		}

		// skip all other files in icloud meta pass
		if ifc.icloudMetaPass {
			continue
		}

		// silently ignore .csv files after icloud meta pass
		if ifc.ICloudTakeout && !ifc.icloudMetaPass && ext == icloudMetadataExt {
			continue
		}

		if matchesBanned(ifc.BannedFiles, name, entry.IsDir()) {
			ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, entry.Name()), 0, fileevent.DiscoveredBanned, "reason", "banned file")
			continue
		}

		if ifc.supportedMedia.IsUseLess(name) {
			ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, entry.Name()), 0, fileevent.DiscoveredUnknown, "reason", "useless file")
			continue
		}

		if ifc.PicasaAlbum && (strings.ToLower(base) == ".picasa.ini" || strings.ToLower(base) == "picasa.ini") {
			a, err := ReadPicasaIni(fsys, name)
			if err != nil {
				ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.ErrorFileAccess, "error", err.Error())
			} else {
				ifc.picasaAlbums.Store(dir, a)
				ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.DiscoveredMetadata, "album", a.Name)
			}
			continue
		}

		mediaType := ifc.supportedMedia.TypeFromExt(ext)

		if mediaType == filetypes.TypeUnknown {
			ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.DiscoveredUnsupported, "reason", "unsupported file type")
			continue
		}

		switch mediaType {
		case filetypes.TypeUseless:
			ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.DiscoveredUnknown)
			continue
		case filetypes.TypeImage, filetypes.TypeVideo:
			// Will be recorded as discovered asset after assetFromFile creates it
		case filetypes.TypeSidecar:
			if ifc.IgnoreSideCarFiles {
				ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.DiscoveredSidecar, "reason", "sidecar file ignored")
				continue
			}
			ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.DiscoveredSidecar)
			continue
		}

		if !ifc.InclusionFlags.IncludedExtensions.Include(ext) {
			// Get file size for discarded asset
			if info, err := entryInfo(fsys, name, entry); err == nil {
				ifc.processor.RecordAssetDiscardedImmediately(ctx, fshelper.FSName(fsys, name), info.Size(), fileevent.DiscardedFiltered, "extension not included")
			}
			continue
		}

		if ifc.InclusionFlags.ExcludedExtensions.Exclude(ext) {
			// Get file size for discarded asset
			if info, err := entryInfo(fsys, name, entry); err == nil {
				ifc.processor.RecordAssetDiscardedImmediately(ctx, fshelper.FSName(fsys, name), info.Size(), fileevent.DiscardedFiltered, "extension excluded")
			}
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// we have a file to process - it's an asset (image or video)
			code := fileevent.DiscoveredImage
			if mediaType == filetypes.TypeVideo {
				code = fileevent.DiscoveredVideo
			}
			a, err := ifc.assetFromEntry(ctx, fsys, name, entry)
			if err != nil {
				fileRef := fshelper.FSName(fsys, name)
				ifc.processor.RecordAssetDiscovered(ctx, fileRef, 0, code)
				ifc.processor.RecordAssetError(ctx, fileRef, 0, fileevent.ErrorFileAccess, err)
				if procErr := ifc.app.ProcessError(err); procErr != nil {
					return procErr
				}
				continue
			}
			if a != nil {
				as = append(as, a)
			}
		}
	}

	if len(as) > 0 {
		eagerDate := ifc.isArchive ||
			ifc.InclusionFlags.DateRange.IsSet() ||
			ifc.ManageBurst != filters.BurstNothing ||
			ifc.ManageHEICJPG != filters.HeicJpgNothing ||
			ifc.ManageRawJPG != filters.RawJPGNothing

		in := make(chan *assets.Asset, 4)
		go func() {
			defer close(in)

			sort.Slice(as, func(i, j int) bool {
				// Sort by radical first
				radicalI := as[i].Radical
				radicalJ := as[j].Radical
				if radicalI != radicalJ {
					return radicalI < radicalJ
				}
				// If radicals are the same, sort by date
				if !as[i].CaptureDate.Equal(as[j].CaptureDate) {
					return as[i].CaptureDate.Before(as[j].CaptureDate)
				}
				return as[i].FileDate.Before(as[j].FileDate)
			})

			for i, a := range as {
				select {
				case <-ctx.Done():
					for _, rem := range as[i:] {
						rem.Close()
					}
					return
				default:
				}

				// check the presence of a JSON file
				jsonName := findSideCarInDir(dirSidecars, a.File.Name(), ".json")
			if jsonName != "" {
				buf, err := fs.ReadFile(fsys, jsonName)
				if err != nil {
					ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, jsonName), 0, fileevent.ErrorFileAccess, "error", err.Error())
				} else {
					if bytes.Contains(buf, []byte("immich-go version")) {
						md := &assets.Metadata{}
						err = jsonsidecar.Read(bytes.NewReader(buf), md)
						if err != nil {
							ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, jsonName), 0, fileevent.ErrorFileAccess, "error", err.Error())
						} else {
							ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, jsonName), 0, fileevent.DiscoveredSidecar)
							md.File = fshelper.FSName(fsys, jsonName)
							a.FromApplication = a.UseMetadata(md) // Force the use of the metadata coming from immich export
							a.OriginalFileName = md.FileName      // Force the name of the file to be the one from the JSON file
						}
					} else {
						ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, jsonName), 0, fileevent.DiscoveredSidecar)
						ifc.app.Log().Warn("JSON file detected but not from immich-go", "file", fshelper.FSName(fsys, jsonName))
					}
				}
			}
			// check the presence of a XMP file
			xmpName := findSideCarInDir(dirSidecars, a.File.Name(), ".xmp")
			if xmpName != "" {
				buf, err := fs.ReadFile(fsys, xmpName)
				if err != nil {
					ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, xmpName), 0, fileevent.ErrorFileAccess, "error", err.Error())
				} else {
					md := &assets.Metadata{}
					err = xmpsidecar.ReadXMP(bytes.NewReader(buf), md)
					if err != nil {
						ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, xmpName), 0, fileevent.ErrorFileAccess, "error", err.Error())
					} else {
						ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, xmpName), 0, fileevent.DiscoveredSidecar)
						md.File = fshelper.FSName(fsys, xmpName)
						a.FromSideCar = a.UseMetadata(md)
					}
				}
			}

			// Read metadata from the file only id needed (date range or take date from filename)
			if ifc.requiresDateInformation {
				// try to get date from icloud takeout meta
				if a.CaptureDate.IsZero() && ifc.ICloudTakeout {
					meta, ok := ifc.icloudMetas.Load(a.OriginalFileName)
					if ok {
						a.FromApplication = &assets.Metadata{
							DateTaken: meta.originalCreationDate,
						}
						a.CaptureDate = a.FromApplication.DateTaken
					}
				}
				if a.CaptureDate.IsZero() {
					sameRadical := (i > 0 && as[i-1].Radical == a.Radical) || (i+1 < len(as) && as[i+1].Radical == a.Radical)
					if eagerDate || sameRadical {
						ifc.loadAssetDate(a, nil)
					} else {
						asset := a
						asset.SetDateLoader(func(header []byte) {
							ifc.loadAssetDate(asset, header)
						})
					}
				}
			}

			if !ifc.InclusionFlags.DateRange.InRange(a.CaptureDate) {
				code := fileevent.DiscoveredImage
				if a.Type == filetypes.TypeVideo {
					code = fileevent.DiscoveredVideo
				}
				a.Close()
				ifc.processor.RecordAssetDiscovered(ctx, a.File, int64(a.FileSize), code)
				ifc.processor.RecordAssetDiscarded(ctx, a.File, int64(a.FileSize), fileevent.DiscardedFiltered, "asset outside date range")
				continue
			}
			if ifc.FolderAsTags {
				t := fsName
				if dir != "." {
					t = path.Join(t, dir)
				}
				if t != "" {
					a.AddTag(t)
				}
			}

			// Manage albums
			if ifc.ImportIntoAlbum != "" {
				a.Albums = []assets.Album{{Title: ifc.ImportIntoAlbum}}
			} else {
				done := false
				if ifc.PicasaAlbum {
					if album, ok := ifc.picasaAlbums.Load(dir); ok {
						a.Albums = []assets.Album{{Title: album.Name, Description: album.Description}}
						done = true
					}
				}
				if ifc.ICloudTakeout {
					if meta, ok := ifc.icloudMetas.Load(a.OriginalFileName); ok {
						a.Albums = meta.albums
						done = true
					}
				}
				if !done && ifc.UsePathAsAlbumName != FolderModeNone && ifc.UsePathAsAlbumName != "" {
					Album := ""
					switch ifc.UsePathAsAlbumName {
					case FolderModeFolder:
						if dir == "." {
							Album = fsName
						} else {
							Album = filepath.Base(dir)
						}
					case FolderModePath:
						parts := []string{}
						if fsName != "" {
							parts = append(parts, fsName)
						}
						if dir != "." {
							parts = append(parts, strings.Split(dir, "/")...)
						}
						Album = strings.Join(parts, ifc.AlbumNamePathSeparator)
					}
					if Album != "" {
						a.Albums = []assets.Album{{Title: Album}}
					}
				}
			}

				select {
				case in <- a:
				case <-ctx.Done():
					for _, rem := range as[i:] {
						rem.Close()
					}
					return
				}
			}
		}()

		gs := groups.NewGrouperPipeline(ctx, ifc.groupers...).PipeGrouper(ctx, in)
		for g := range gs {
			if ctx.Err() != nil {
				for _, a := range g.Assets {
					a.Close()
				}
				return ctx.Err()
			}
			for _, a := range g.Assets {
				code := fileevent.DiscoveredImage
				if a.Type == filetypes.TypeVideo {
					code = fileevent.DiscoveredVideo
				}
				ifc.processor.RecordAssetDiscovered(ctx, a.File, int64(a.FileSize), code)
			}
			select {
			case gOut <- g:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	// process the left over dirs
	for _, entry := range entries {
		base := entry.Name()
		name := path.Join(dir, base)
		if entry.IsDir() {
			if matchesBanned(ifc.BannedFiles, name, true) {
				ifc.processor.RecordNonAsset(ctx, fshelper.FSName(fsys, name), 0, fileevent.DiscoveredBanned, "reason", "banned folder")
				continue // Skip this folder, no error
			}
			if ifc.Recursive && entry.Name() != "." {
				ifc.concurrentParseDir(ctx, fsys, name, gOut)
			}
			continue
		}
	}
	return nil
}

func findSideCarInDir(dirSidecars map[string]string, assetPath string, ext string) string {
	lowExt := strings.ToLower(ext)
	base := path.Base(assetPath)
	if p, ok := dirSidecars[base+lowExt]; ok {
		return p
	}
	mediaExt := path.Ext(base)
	if !filetypes.DefaultSupportedMedia.IsMedia(mediaExt) {
		return ""
	}
	stem := strings.TrimSuffix(base, mediaExt)
	if p, ok := dirSidecars[stem+lowExt]; ok {
		return p
	}
	return ""
}

func matchesBanned(list namematcher.List, name string, isDir bool) bool {
	trimmed := strings.TrimSuffix(name, "/")
	if isDir {
		if list.MatchDir(name) {
			return true
		}
		if trimmed != name && list.MatchDir(trimmed) {
			return true
		}
		return false
	}
	if list.MatchFile(name) {
		return true
	}
	if trimmed != name && list.MatchFile(trimmed) {
		return true
	}
	return false
}

func entryInfo(fsys fs.FS, name string, entry fs.DirEntry) (fs.FileInfo, error) {
	if entry != nil {
		if info, err := entry.Info(); err == nil {
			return info, nil
		}
	}
	return fs.Stat(fsys, name)
}

func (ifc *ImportFolderCmd) loadAssetDate(a *assets.Asset, header []byte) {
	if !a.CaptureDate.IsZero() {
		return
	}
	var md *assets.Metadata
	var err error
	if len(header) > 0 {
		md, err = exif.GetMetaData(bytes.NewReader(header), a.Ext, ifc.tz)
		if err == nil {
			a.FromSourceFile = a.UseMetadata(md)
		} else if a.FileSize > len(header) {
			f, openErr := a.File.Open()
			if openErr == nil {
				md, err = exif.GetMetaData(f, a.Ext, ifc.tz)
				if err == nil {
					a.FromSourceFile = a.UseMetadata(md)
				}
				_ = f.Close()
			}
		}
	} else {
		f, openErr := a.File.Open()
		if openErr == nil {
			md, err = exif.GetMetaData(f, a.Ext, ifc.tz)
			if err == nil {
				a.FromSourceFile = a.UseMetadata(md)
			}
			_ = f.Close()
		}
	}
	if (md == nil || md.DateTaken.IsZero()) && !a.Taken.IsZero() && ifc.TakeDateFromFilename {
		// no exif, but we have a date in the filename and the TakeDateFromFilename is set
		a.FromApplication = &assets.Metadata{
			DateTaken: a.Taken,
		}
		a.CaptureDate = a.FromApplication.DateTaken
	}
}

func (ifc *ImportFolderCmd) assetFromEntry(_ context.Context, fsys fs.FS, name string, entry fs.DirEntry) (*assets.Asset, error) {
	a := &assets.Asset{
		File:             fshelper.FSName(fsys, name),
		OriginalFileName: filepath.Base(name),
	}
	i, err := entryInfo(fsys, name, entry)
	if err != nil {
		a.Close()
		return nil, err
	}
	a.FileSize = int(i.Size())
	a.FileDate = i.ModTime()

	n := path.Join(path.Dir(name), a.OriginalFileName)
	if fsys, ok := fsys.(interface{ Name() string }); ok {
		n = path.Join(fsys.Name(), n)
	}

	a.SetNameInfo(ifc.infoCollector.GetInfo(n))
	return a, nil
}

// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/structutil"
)

var (
	ErrPathNotDirectory = errors.New("folder path not a directory")
	ErrPathMissing      = errors.New("folder path missing")
	ErrMarkerMissing    = errors.New("folder marker missing (this indicates potential data loss, search docs/forum to get information about how to proceed)")
)

const (
	DefaultMarkerName          = ".stfolder"
	EncryptionTokenName        = "syncthing-encryption_password_token" //nolint: gosec
	maxConcurrentWritesDefault = 16
	maxConcurrentWritesLimit   = 256
)

type FolderDeviceConfiguration struct {
	DeviceID           protocol.DeviceID `json:"deviceID" xml:"id,attr"`
	IntroducedBy       protocol.DeviceID `json:"introducedBy" xml:"introducedBy,attr"`
	EncryptionPassword string            `json:"encryptionPassword" xml:"encryptionPassword"`
}

type FolderConfiguration struct {
	ID                      string                      `json:"id" xml:"id,attr" nodefault:"true"`
	Label                   string                      `json:"label" xml:"label,attr" restart:"false"`
	FilesystemType          FilesystemType              `json:"filesystemType" xml:"filesystemType" default:"basic"`
	Path                    string                      `json:"path" xml:"path,attr"`
	Type                    FolderType                  `json:"type" xml:"type,attr"`
	Devices                 []FolderDeviceConfiguration `json:"devices" xml:"device"`
	Group                   string                      `json:"group" xml:"group,attr,omitempty" restart:"false"`
	RescanIntervalS         int                         `json:"rescanIntervalS" xml:"rescanIntervalS,attr" default:"3600"`
	FSWatcherEnabled        bool                        `json:"fsWatcherEnabled" xml:"fsWatcherEnabled,attr" default:"true"`
	FSWatcherDelayS         float64                     `json:"fsWatcherDelayS" xml:"fsWatcherDelayS,attr" default:"10"`
	FSWatcherTimeoutS       float64                     `json:"fsWatcherTimeoutS" xml:"fsWatcherTimeoutS,attr"`
	IgnorePerms             bool                        `json:"ignorePerms" xml:"ignorePerms,attr"`
	AutoNormalize           bool                        `json:"autoNormalize" xml:"autoNormalize,attr" default:"true"`
	MinDiskFree             Size                        `json:"minDiskFree" xml:"minDiskFree" default:"1 %"`
	Versioning              VersioningConfiguration     `json:"versioning" xml:"versioning"`
	Copiers                 int                         `json:"copiers" xml:"copiers"`
	PullerMaxPendingKiB     int                         `json:"pullerMaxPendingKiB" xml:"pullerMaxPendingKiB"`
	Hashers                 int                         `json:"hashers" xml:"hashers"`
	Order                   PullOrder                   `json:"order" xml:"order"`
	IgnoreDelete            bool                        `json:"ignoreDelete" xml:"ignoreDelete"`
	ScanProgressIntervalS   int                         `json:"scanProgressIntervalS" xml:"scanProgressIntervalS"`
	PullerPauseS            int                         `json:"pullerPauseS" xml:"pullerPauseS"`
	PullerDelayS            float64                     `json:"pullerDelayS" xml:"pullerDelayS" default:"1"`
	MaxConflicts            int                         `json:"maxConflicts" xml:"maxConflicts" default:"10"`
	DisableSparseFiles      bool                        `json:"disableSparseFiles" xml:"disableSparseFiles"`
	Paused                  bool                        `json:"paused" xml:"paused"`
	MarkerName              string                      `json:"markerName" xml:"markerName"`
	CopyOwnershipFromParent bool                        `json:"copyOwnershipFromParent" xml:"copyOwnershipFromParent"`
	RawModTimeWindowS       int                         `json:"modTimeWindowS" xml:"modTimeWindowS"`
	MaxConcurrentWrites     int                         `json:"maxConcurrentWrites" xml:"maxConcurrentWrites" default:"0"`
	DisableFsync            bool                        `json:"disableFsync" xml:"disableFsync"`
	BlockPullOrder          BlockPullOrder              `json:"blockPullOrder" xml:"blockPullOrder"`
	CopyRangeMethod         CopyRangeMethod             `json:"copyRangeMethod" xml:"copyRangeMethod" default:"standard"`
	CaseSensitiveFS         bool                        `json:"caseSensitiveFS" xml:"caseSensitiveFS"`
	JunctionsAsDirs         bool                        `json:"junctionsAsDirs" xml:"junctionsAsDirs"`
	SyncOwnership           bool                        `json:"syncOwnership" xml:"syncOwnership"`
	SendOwnership           bool                        `json:"sendOwnership" xml:"sendOwnership"`
	SyncXattrs              bool                        `json:"syncXattrs" xml:"syncXattrs"`
	SendXattrs              bool                        `json:"sendXattrs" xml:"sendXattrs"`
	BlockIndexing           bool                        `json:"blockIndexing" xml:"blockIndexing" default:"true"`
	XattrFilter             XattrFilter                 `json:"xattrFilter" xml:"xattrFilter"`

	// On-demand ("online-only") files, tether extension. Only valid for
	// send-receive folders. Path is used by the sync engine; users access
	// the folder through OnDemandView, a bind mount where placeholders are
	// hydrated on open.
	OnDemand          bool     `json:"onDemand" xml:"onDemand"`
	OnDemandView      string   `json:"onDemandView" xml:"onDemandView"`
	PinPatterns       []string `json:"pinPatterns" xml:"pinPattern" restart:"false"`
	CacheBudget       Size     `json:"cacheBudget" xml:"cacheBudget" default:"0" restart:"false"`
	HydrationDenyExes []string `json:"hydrationDenyExes" xml:"hydrationDenyExe" restart:"false"`
	HydrationTimeoutS int      `json:"hydrationTimeoutS" xml:"hydrationTimeoutS" default:"60" restart:"false"`
	// When an application opens a placeholder, its sibling placeholders up to
	// this size are downloaded in the background (0 disables).
	PrefetchMaxFileKiB  int `json:"prefetchMaxFileKiB" xml:"prefetchMaxFileKiB" default:"256" restart:"false"`
	PrefetchConcurrency int `json:"prefetchConcurrency" xml:"prefetchConcurrency" default:"64"`
	// Ahead of an application walking the tree (grep -r, find -exec, a
	// build), the placeholders it will open next, up to this size, are
	// downloaded in the background (0 disables), up to CrawlPrefetchMaxMiB
	// per walk.
	CrawlPrefetchMaxFileKiB int `json:"crawlPrefetchMaxFileKiB" xml:"crawlPrefetchMaxFileKiB" default:"1024" restart:"false"`
	CrawlPrefetchMaxMiB     int `json:"crawlPrefetchMaxMiB" xml:"crawlPrefetchMaxMiB" default:"1024" restart:"false"`

	// Legacy deprecated
	DeprecatedReadOnly       bool    `json:"-" xml:"ro,attr,omitempty"`        // Deprecated: Do not use.
	DeprecatedMinDiskFreePct float64 `json:"-" xml:"minDiskFreePct,omitempty"` // Deprecated: Do not use.
	DeprecatedPullers        int     `json:"-" xml:"pullers,omitempty"`        // Deprecated: Do not use.
	DeprecatedScanOwnership  bool    `json:"-" xml:"scanOwnership,omitempty"`  // Deprecated: Do not use.
}

// Extended attribute filter. This is a list of patterns to match (glob
// style), each with an action (permit or deny). First match is used. If the
// filter is empty, all strings are permitted. If the filter is non-empty,
// the default action becomes deny. To counter this, you can use the "*"
// pattern to match all strings at the end of the filter. There are also
// limits on the size of accepted attributes.
type XattrFilter struct {
	Entries            []XattrFilterEntry `json:"entries" xml:"entry"`
	MaxSingleEntrySize int                `json:"maxSingleEntrySize" xml:"maxSingleEntrySize" default:"1024"`
	MaxTotalSize       int                `json:"maxTotalSize" xml:"maxTotalSize" default:"4096"`
}

type XattrFilterEntry struct {
	Match  string `json:"match" xml:"match,attr"`
	Permit bool   `json:"permit" xml:"permit,attr"`
}

func (f FolderConfiguration) Copy() FolderConfiguration {
	c := f
	c.Devices = make([]FolderDeviceConfiguration, len(f.Devices))
	copy(c.Devices, f.Devices)
	c.Versioning = f.Versioning.Copy()
	c.PinPatterns = slices.Clone(f.PinPatterns)
	c.HydrationDenyExes = slices.Clone(f.HydrationDenyExes)
	return c
}

// Filesystem creates a filesystem for the path and options of this folder.
// The fset parameter may be nil, in which case no mtime handling on top of
// the filesystem is provided.
func (f FolderConfiguration) Filesystem(extraOpts ...fs.Option) fs.Filesystem {
	// This is intentionally not a pointer method, because things like
	// cfg.Folders["default"].Filesystem(nil) should be valid.
	var opts []fs.Option
	if f.FilesystemType == FilesystemTypeBasic && f.JunctionsAsDirs {
		opts = append(opts, new(fs.OptionJunctionsAsDirs))
	}
	if !f.CaseSensitiveFS {
		opts = append(opts, new(fs.OptionDetectCaseConflicts))
	}
	opts = append(opts, extraOpts...)
	return fs.NewFilesystem(f.FilesystemType.ToFS(), f.Path, opts...)
}

func (f FolderConfiguration) ModTimeWindow() time.Duration {
	dur := time.Duration(f.RawModTimeWindowS) * time.Second
	if f.RawModTimeWindowS < 1 && build.IsAndroid {
		if usage, err := disk.Usage(f.Filesystem().URI()); err != nil {
			dur = 2 * time.Second
			l.Debugf(`Detecting FS at "%v" on android: Setting mtime window to 2s: err == "%v"`, f.Path, err)
		} else if strings.HasPrefix(strings.ToLower(usage.Fstype), "ext2") || strings.HasPrefix(strings.ToLower(usage.Fstype), "ext3") || strings.HasPrefix(strings.ToLower(usage.Fstype), "ext4") {
			l.Debugf(`Detecting FS at %v on android: Leaving mtime window at 0: usage.Fstype == "%v"`, f.Path, usage.Fstype)
		} else {
			dur = 2 * time.Second
			l.Debugf(`Detecting FS at "%v" on android: Setting mtime window to 2s: usage.Fstype == "%v"`, f.Path, usage.Fstype)
		}
	}
	return dur
}

func (f *FolderConfiguration) CreateMarker() error {
	if err := f.CheckPath(); !errors.Is(err, ErrMarkerMissing) {
		return err
	}
	if f.MarkerName != DefaultMarkerName {
		// Folder uses a non-default marker so we shouldn't mess with it.
		// Pretend we created it and let the subsequent health checks sort
		// out the actual situation.
		return nil
	}

	ffs := f.Filesystem()

	// Create the marker as a directory
	err := ffs.Mkdir(DefaultMarkerName, fs.ModePerm)
	if err != nil {
		return err
	}

	// Create a file inside it, reducing the risk of the marker directory
	// being removed by automated cleanup tools.
	markerFile := filepath.Join(DefaultMarkerName, f.markerFilename())
	if err := fs.WriteFile(ffs, markerFile, f.markerContents(), 0o666); err != nil {
		return err
	}

	// Sync & hide the containing directory
	if dir, err := ffs.Open("."); err != nil {
		l.Debugln("folder marker: open . failed:", err)
	} else if err := dir.Sync(); err != nil {
		l.Debugln("folder marker: fsync . failed:", err)
	}
	ffs.Hide(DefaultMarkerName)

	return nil
}

func (f *FolderConfiguration) RemoveMarker() error {
	ffs := f.Filesystem()
	_ = ffs.Remove(filepath.Join(DefaultMarkerName, f.markerFilename()))
	return ffs.Remove(DefaultMarkerName)
}

func (f *FolderConfiguration) markerFilename() string {
	h := sha256.Sum256([]byte(f.ID))
	return fmt.Sprintf("syncthing-folder-%x.txt", h[:3])
}

func (f *FolderConfiguration) markerContents() []byte {
	var buf bytes.Buffer
	buf.WriteString("# This directory is a Syncthing folder marker.\n# Do not delete.\n\n")
	fmt.Fprintf(&buf, "folderID: %s\n", f.ID)
	fmt.Fprintf(&buf, "created: %s\n", time.Now().Format(time.RFC3339))
	return buf.Bytes()
}

// CheckPath returns nil if the folder root exists and contains the marker file
func (f *FolderConfiguration) CheckPath() error {
	return f.checkFilesystemPath(f.Filesystem(), ".")
}

func (f *FolderConfiguration) checkFilesystemPath(ffs fs.Filesystem, path string) error {
	fi, err := ffs.Stat(path)
	if err != nil {
		if !fs.IsNotExist(err) {
			return err
		}
		return ErrPathMissing
	}

	// Users might have the root directory as a symlink or reparse point.
	// Furthermore, OneDrive bullcrap uses a magic reparse point to the cloudz...
	// Yet it's impossible for this to happen, as filesystem adds a trailing
	// path separator to the root, so even if you point the filesystem at a file
	// Stat ends up calling stat on C:\dir\file\ which, fails with "is not a directory"
	// in the error check above, and we don't even get to here.
	if !fi.IsDir() && !fi.IsSymlink() {
		return ErrPathNotDirectory
	}

	_, err = ffs.Stat(filepath.Join(path, f.MarkerName))
	if err != nil {
		if !fs.IsNotExist(err) {
			return err
		}
		return ErrMarkerMissing
	}

	return nil
}

func (f *FolderConfiguration) CreateRoot() (err error) {
	filesystem := f.Filesystem()

	if _, err = filesystem.Stat("."); fs.IsNotExist(err) {
		err = filesystem.MkdirAll(".", fs.ModePerm)
	}

	return err
}

func (f FolderConfiguration) Description() string {
	if f.Label == "" {
		return f.ID
	}
	return fmt.Sprintf("%q (%s)", f.Label, f.ID)
}

func (f FolderConfiguration) LogAttr() slog.Attr {
	if f.Label == "" || f.Label == f.ID {
		return slog.Group("folder", slog.String("id", f.ID), slog.String("type", f.Type.String()))
	}
	return slog.Group("folder", slog.String("label", f.Label), slog.String("id", f.ID), slog.String("type", f.Type.String()))
}

func (f *FolderConfiguration) DeviceIDs() []protocol.DeviceID {
	deviceIDs := make([]protocol.DeviceID, len(f.Devices))
	for i, n := range f.Devices {
		deviceIDs[i] = n.DeviceID
	}
	return deviceIDs
}

func (f *FolderConfiguration) prepare(myID protocol.DeviceID, existingDevices map[protocol.DeviceID]*DeviceConfiguration) {
	// Ensure that
	// - any loose devices are not present in the wrong places
	// - there are no duplicate devices
	// - we are part of the devices
	// - folder is not shared in trusted mode with an untrusted device
	f.Devices = ensureExistingDevices(f.Devices, existingDevices)
	f.Devices = ensureNoDuplicateFolderDevices(f.Devices)
	f.Devices = ensureDevicePresent(f.Devices, myID)
	f.Devices = ensureNoUntrustedTrustingSharing(f, f.Devices, existingDevices)

	slices.SortFunc(f.Devices, func(a, b FolderDeviceConfiguration) int {
		return a.DeviceID.Compare(b.DeviceID)
	})

	if f.RescanIntervalS > MaxRescanIntervalS {
		f.RescanIntervalS = MaxRescanIntervalS
	} else if f.RescanIntervalS < 0 {
		f.RescanIntervalS = 0
	}

	if f.FSWatcherDelayS <= 0 {
		f.FSWatcherEnabled = false
		f.FSWatcherDelayS = 10
	} else if f.FSWatcherDelayS < 0.01 {
		f.FSWatcherDelayS = 0.01
	}

	if f.Versioning.CleanupIntervalS > MaxRescanIntervalS {
		f.Versioning.CleanupIntervalS = MaxRescanIntervalS
	} else if f.Versioning.CleanupIntervalS < 0 {
		f.Versioning.CleanupIntervalS = 0
	}

	if f.MarkerName == "" {
		f.MarkerName = DefaultMarkerName
	}

	if f.MaxConcurrentWrites <= 0 {
		f.MaxConcurrentWrites = maxConcurrentWritesDefault
	} else if f.MaxConcurrentWrites > maxConcurrentWritesLimit {
		f.MaxConcurrentWrites = maxConcurrentWritesLimit
	}

	if f.Type == FolderTypeReceiveEncrypted {
		f.IgnorePerms = true
	}

	// An unusable on-demand setting is kept as is: the folder then refuses
	// to start (see folder.getHealthErrorWithoutIgnores). Clearing it would
	// turn the folder into a normal one that downloads everything.
}

// deriveOnDemandView fills in the view of an on-demand folder configured
// with only a path, as a folder accepted from a share offer is when the
// defaults have onDemand set. The chosen path becomes the view, where the
// user sees the files, and the synced data moves to lowerRoot/<folder ID>.
func (f *FolderConfiguration) deriveOnDemandView(lowerRoot string) {
	if !f.OnDemand || f.OnDemandView != "" || lowerRoot == "" {
		return
	}
	view, err := fs.ExpandTilde(f.Path)
	if err != nil {
		return
	}
	view = filepath.Clean(view)
	if view == lowerRoot || strings.HasPrefix(view, lowerRoot+string(filepath.Separator)) {
		// Already a derived path (e.g. the view was cleared by hand);
		// leave it for the folder to report.
		return
	}
	name := fs.SanitizePath(f.ID)
	if name == "" {
		return
	}
	f.OnDemandView = view
	f.Path = filepath.Join(lowerRoot, name)
}

// RequiresRestartOnly returns a copy with only the attributes that require
// restart on change.
func (f FolderConfiguration) RequiresRestartOnly() FolderConfiguration {
	copy := f

	// Manual handling for things that are not taken care of by the tag
	// copier, yet should not cause a restart.

	blank := FolderConfiguration{}
	copyMatchingTag(&blank, &copy, "restart", func(v string) bool {
		if len(v) > 0 && v != "false" {
			panic(fmt.Sprintf(`unexpected tag value: %s. expected untagged or "false"`, v))
		}
		return v == "false"
	})
	return copy
}

func (f *FolderConfiguration) Device(device protocol.DeviceID) (FolderDeviceConfiguration, bool) {
	for _, dev := range f.Devices {
		if dev.DeviceID == device {
			return dev, true
		}
	}
	return FolderDeviceConfiguration{}, false
}

func (f *FolderConfiguration) SharedWith(device protocol.DeviceID) bool {
	_, ok := f.Device(device)
	return ok
}

func (f *FolderConfiguration) CheckAvailableSpace(req uint64) error {
	val := f.MinDiskFree.BaseValue()
	if val <= 0 {
		return nil
	}
	fs := f.Filesystem()
	usage, err := fs.Usage(".")
	if err != nil {
		return nil //nolint: nilerr
	}
	if err := checkAvailableSpace(req, f.MinDiskFree, usage); err != nil {
		return fmt.Errorf("insufficient space in folder %v (%v): %w", f.Description(), fs.URI(), err)
	}
	return nil
}

func (f XattrFilter) Permit(s string) bool {
	if len(f.Entries) == 0 {
		return true
	}

	for _, entry := range f.Entries {
		if ok, _ := path.Match(entry.Match, s); ok {
			return entry.Permit
		}
	}
	return false
}

func (f XattrFilter) GetMaxSingleEntrySize() int {
	return f.MaxSingleEntrySize
}

func (f XattrFilter) GetMaxTotalSize() int {
	return f.MaxTotalSize
}

func (f *FolderConfiguration) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	structutil.SetDefaults(f)

	// avoid recursing into this method
	type noCustomUnmarshal FolderConfiguration
	ptr := (*noCustomUnmarshal)(f)

	return d.DecodeElement(ptr, &start)
}

# Immich-Go: Upload Your Photos to Your Immich Server

**Immich-Go** is an open-source tool designed to streamline uploading large photo collections to your self-hosted Immich server.

> ✅ **Immich V3+ Ready** — Actively maintained fork with reliability, performance, memory, and TUI fixes for large photo libraries and network/cloud mounts.<br>
> ⚠️ Keep a backup copy of your files for safety.<br>

## 🔧 What's Improved & Fixed in This Repo

This repository ([`supernovae/immich-go`](https://github.com/supernovae/immich-go)) includes a comprehensive set of fixes and performance improvements for large-scale imports and Immich v3+:

- **Fast Startup & Lazy Directory Scanning**:
  - Validates server connectivity (`PingServer`, authentication, and server info) with a bounded startup check *before* scanning local or remote paths.
  - `VirtualGlobFS` now evaluates directories lazily on demand instead of walking the entire filesystem tree before launching the TUI.
- **Duplicate-Heavy & Network/FUSE Mount Optimization (`rclone`, NFS, SMB)**:
  - Defers EXIF metadata reading until *after* SHA-1 checksum deduplication—files already on the server or duplicated locally are never opened a second time for EXIF parsing.
  - Captures the first 64 KB header while computing the SHA-1 checksum so non-duplicate JPEGs extract EXIF in memory without reopening the file.
  - Reuses `DirEntry.Info()` from directory reads and reuses open file handles in `CacheReader` instead of closing and reopening files before upload.
  - Paces folder discovery with bounded pipeline buffers so `Pending` reflects active in-flight assets rather than spiking by hundreds or thousands of items at the end of a batch.
- **TUI Progress & Lifecycle Tracking Fixes**:
  - Added the live `Uploading:` progress gauge to the TUI footer across all upload modes (`from-folder`, `from-icloud`, `from-picasa`, and `from-google-photos`), dynamically refreshing as pending items are discovered and processed.
  - Fixed `Pending` counter leaks when file reads fail during checksum calculation, when `--overwrite` replaces existing server assets, when `--manage-epson-fastfoto` encounters video/burst files, and when Google Takeout archives contain duplicate or unmatched files.
  - Clean `Ctrl+C` / `Ctrl+Q` cancellation that stops queued workers immediately without logging spurious upload errors or hanging the terminal.
- **Large-Import Resilience & API Retries**:
  - Added exponential backoff + jitter retries for transient HTTP/5xx/429 server errors (`--api-retries`, default `3`; `--api-retry-delay`, default `500ms`) and optional upload pacing (`--upload-delay`, e.g. `50ms`).
  - Fixed `io.ErrClosedPipe` handling when Immich closes the multipart stream early on duplicate detection or HTTP errors (such as `413 Request Entity Too Large`).
  - Changed `--on-errors` default to `continue` so single-file or single-directory errors do not abort multi-hour batch imports.
  - Replaced `immichIndex` panics on duplicate server checksums with warnings.
- **Memory & File Descriptor Management**:
  - Compacted in-memory indexed server/local `Asset` structs, evicted finalized records from `AssetTracker`, and released processed Google Takeout catalog maps to prevent multi-GB RAM growth on 100,000+ asset libraries.
  - Replaced the global `CollectionCache` channel bottleneck with per-collection locking and ensured buffered album/tag updates flush cleanly in `--no-ui` mode.
  - Replaced $O(N^2)$ `fs.Glob` sidecar lookups in `from-folder` with an $O(1)$ per-directory map and fixed Google Takeout fast-track JSON matching.

## 🌟 Key Features

- **Immich V3+ Support**: Full compatibility with Immich V3 API endpoints and error formats
- **Simple Installation**: Single static Go binary—no NodeJS or Docker required
- **Multiple Sources**: Upload from Google Photos Takeouts, iCloud, local folders, ZIP archives, and other Immich servers
- **Large Collections**: Built to handle 100,000+ photos and cloud/network mounts reliably
- **Smart Management**: Duplicate detection, burst photo stacking, RAW+JPEG and HEIC+JPEG handling
- **Cross-Platform**: Works on Linux, macOS, Windows, and FreeBSD

## 🚀 Quick Start

### 1. Build or Install
```bash
git clone https://github.com/supernovae/immich-go.git
cd immich-go
go build -o immich-go .
```

### 2. Basic Usage
```bash
# Upload photos from a local folder
immich-go upload from-folder --server=http://your-ip:2283 --api-key=your-api-key /path/to/your/photos

# Upload Google Photos takeout
immich-go upload from-google-photos --server=http://your-ip:2283 --api-key=your-api-key /path/to/takeout-*.zip

# Archive photos from Immich server
immich-go archive from-immich --from-server=http://your-ip:2283 --from-api-key=your-api-key --write-to-folder=/path/to/archive

# Upload with custom concurrency, retry settings, or upload pacing
immich-go upload from-folder --server=https://photos.example.com --api-key=your-api-key --concurrent-tasks=4 --api-retries=3 --upload-delay=25ms /path/to/your/photos
```

### 3. Requirements
- A running Immich server with API access
- API key with appropriate permissions ([see full list](docs/installation.md#api-permissions))

> ⚠️ **Breaking Change (v0.32.0)**: The `ReplaceAsset` API method has been removed. The `asset.replace` permission is no longer needed. If your API key includes it, it will be ignored by the server. Use `asset.copy` instead for asset duplication needs.

## 🙈 Skip System Files

- Use `--ban-file` to exclude junk artifacts. Patterns ending with `/` apply to directories (for example, `--ban-file .Spotlight-V100/`), while patterns without the trailing slash apply to individual files (for example, `--ban-file .DS_Store`).
- Immich-Go ships with sensible defaults that already skip common clutter such as `@eaDir/`, `@__thumb/`, `SYNOFILE_THUMB_*.*`, `Lightroom Catalog/`, `thumbnails/`, `.DS_Store`, `/._*`, `.Spotlight-V100/`, `.photostructure/`, and `Recently Deleted/`.
- Add additional patterns as needed to keep uploads focused on real photos. See the [banned files reference](docs/technical.md#banned-files) for details.

## 📚 Documentation

| Topic | Description |
|-------|-------------|
| [Installation](docs/installation.md) | Detailed installation instructions for all platforms |
| [Commands](docs/commands/) | Complete command reference and options |
| [Configuration](docs/configuration.md) | Configuration options and environment variables |
| [Examples](docs/examples.md) | Common use cases and practical examples |
| [Best Practices](docs/best-practices.md) | Tips for optimal performance and reliability |
| [Technical Details](docs/technical.md) | File processing, metadata handling, and advanced features |
| [Upload Commands Overview](docs/upload-commands-overview.md) | How `immich-go` processes files from different sources |
| [Release Notes](docs/releases/) | Version history and release notes |

## ✨ How immich-go Works

`immich-go` offers a versatile set of commands to handle your photo and video uploads. Whether you're uploading from a simple folder, migrating from a Google Photos Takeout, or transferring assets between Immich servers, the tool provides intelligent features to preserve your metadata and organization.

Here's a brief overview of the main upload commands:

*   **`from-folder`**: The basic command for uploading from any local folder. It can create albums from your directory structure and read XMP sidecar files.
*   **`from-google-photos`**: A powerful command to migrate from a Google Photos Takeout. It intelligently matches photos with their JSON metadata to preserve albums, descriptions, and locations.
*   **`from-immich`**: A server-to-server migration tool that allows you to copy assets between two Immich instances with fine-grained filtering.
*   **`from-picasa`**: A specialized version of `from-folder` that automatically reads `.picasa.ini` files to restore your Picasa album organization.
*   **`from-icloud`**: Another specialized command that handles the complexity of an iCloud Photos takeout, correctly identifying creation dates and album structures from the included CSV files.

### Leveraging Immich's Features

`immich-go` is more than just an uploader; it intelligently interacts with the Immich server to preserve your library's structure:

*   **Albums and Tags**: Automatically creates albums and tags on the server to match your source organization.
*   **Stacking**: Groups related images, like RAW+JPEG pairs or photo bursts, into stacks.
*   **Duplicate Detection**: Avoids re-uploading files that already exist on the server.
*   **Efficient Uploads**: Can pause Immich's background jobs (like thumbnailing) during an upload for better performance.

For a detailed explanation of how each upload command works, please see the [Upload Commands Overview](docs/upload-commands-overview.md).

## 🎯 Popular Use Cases

- **Google Photos Migration**: [Complete guide](docs/best-practices.md#google-photos-migration)
- **iCloud Import**: [Step-by-step instructions](docs/examples.md#icloud-import)
- **Server Migration**: [Transfer between Immich instances](docs/examples.md#server-migration)
- **Bulk Organization**: [Stacking and tagging strategies](docs/best-practices.md#organization-strategies)

## 💡 Support the Project

- [GitHub Sponsor](https://github.com/sponsors/simulot)
- [PayPal Donation](https://www.paypal.com/donate/?hosted_button_id=VGU2SQE88T2T4)


## 🤝 Contributing

Contributions are welcome! Please see our [contributing guidelines](CONTRIBUTING.md) for details.

## 📄 License

This project is licensed under the terms specified in the [LICENSE](LICENSE) file.

---

**Need help?** Check our [documentation](docs/) or open an issue on GitHub.

// Package filebrowse serves WorkDir directory listings and file downloads
// over the agent bidi stream. All paths are jailed under the strategy root
// via os.OpenRoot.
package filebrowse

const (
	// ChunkSize is the FileChunk payload size (well under the gRPC 4 MiB cap).
	ChunkSize = 64 * 1024

	// MaxSingleFileBytes is the hard cap for a raw single-file fetch.
	MaxSingleFileBytes = 256 << 20 // 256 MiB

	// MaxTarballFiles is the maximum number of regular files in one archive.
	MaxTarballFiles = 500

	// MaxTarballUncompressedBytes is the sum of file sizes before compression.
	MaxTarballUncompressedBytes = 512 << 20 // 512 MiB

	// MaxConcurrentTransfers bounds simultaneous ListDir/FetchFiles handlers.
	MaxConcurrentTransfers = 2
)

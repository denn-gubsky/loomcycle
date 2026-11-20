package workspace

import "time"

// The ops the Workspace tool sends (RFC GW P1): a command, and the five file
// operations of the Read, Write, Edit, Glob and Grep tools run by the worker
// on the workspace's directory through internal/fsops. Each takes its JSON
// arguments below; write and edit send theirs as the op's input, since a file
// body can be larger than an OpStart should carry.
const (
	OpExec  = "exec"
	OpRead  = "read"
	OpWrite = "write"
	OpEdit  = "edit"
	OpGlob  = "glob"
	OpGrep  = "grep"
)

// ExecArgs are an exec op's arguments. TimeoutMS is decided by the core from
// the operator's workspace.exec bounds; the worker refuses one that is not
// positive.
type ExecArgs struct {
	Command   string `json:"command"`
	Cwd       string `json:"cwd,omitempty"`
	TimeoutMS int64  `json:"timeout_ms"`
}

// ExecAnswer is an exec op's output.
type ExecAnswer struct {
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timed_out,omitempty"`
}

// ReadArgs are the Read tool's arguments without volume.
type ReadArgs struct {
	Path        string `json:"path"`
	Offset      *int   `json:"offset,omitempty"`
	Limit       *int   `json:"limit,omitempty"`
	LineNumbers bool   `json:"line_numbers,omitempty"`
}

// WriteArgs are the Write tool's arguments without volume.
type WriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// EditArgs are the Edit tool's arguments without volume.
type EditArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// GlobArgs are the Glob tool's arguments without volume.
type GlobArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

// GrepArgs are the Grep tool's arguments without volume.
type GrepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path,omitempty"`
	Glob            string `json:"glob,omitempty"`
	OutputMode      string `json:"output_mode,omitempty"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty"`
	Multiline       bool   `json:"multiline,omitempty"`
	HeadLimit       int    `json:"head_limit,omitempty"`
	After           int    `json:"-A,omitempty"`
	Before          int    `json:"-B,omitempty"`
	Context         int    `json:"-C,omitempty"`
}

// FileAnswer is a file op's output: the text the file tool would return, with
// paths relative to the workspace, and for glob and grep how many items it
// holds.
type FileAnswer struct {
	Text  string `json:"text"`
	Count *int   `json:"count,omitempty"`
}

// The raw-byte file ops (RFC GW P2): what the File tool uses to move files in
// and out of a workspace by reference. A file's bytes ride the op stream's
// Chunk frames as they are, never inside JSON: get sends them as its output,
// put takes them as its input. Every other op takes small JSON arguments and
// answers small JSON.
//
// Every path is relative to the workspace's root, with '/' as the separator;
// the worker refuses an absolute path, a `..` and a link anywhere on the
// path (links are reported by fstat and fls, never followed).
const (
	OpFStat = "fstat"
	OpFList = "fls"
	// OpGet streams a file, or a byte range of it, out.
	OpGet = "get"
	// OpPut streams a file in: written to a temporary file in the
	// workspace, synced and renamed into place, or nothing is left.
	OpPut   = "put"
	OpMkdir = "mkdir"
	// OpFRemove removes a file or a folder, never the workspace's root. Not
	// "remove": that name is the core's removal of a whole workspace.
	OpFRemove = "fremove"
	// OpStage makes a hidden staging folder in the workspace; puts and
	// mkdirs name it to build a folder there, and commit renames it into
	// place at once (discard drops it).
	OpStage   = "stage"
	OpCommit  = "commit"
	OpDiscard = "discard"
	// OpCopyLocal copies a file or a folder from another workspace on the
	// same worker (or the same one) into this one.
	OpCopyLocal = "copy_local"
)

// DefaultDiskQuotaBytes is a workspace's disk quota when its open named none
// (a workspace opened before quotas existed): config workspace.disk_quota_mb's
// default.
const DefaultDiskQuotaBytes = 2048 << 20

// MaxDiskQuotaBytes bounds a workspace's disk quota: 16 TiB.
const MaxDiskQuotaBytes = 16 << 40

// OpenArgs are an open op's arguments; none at all is the defaults.
type OpenArgs struct {
	// DiskQuotaBytes bounds what the workspace may hold; zero is
	// DefaultDiskQuotaBytes.
	DiskQuotaBytes int64 `json:"disk_quota_bytes,omitempty"`
}

// The kinds of entry fstat and fls report.
const (
	EntryFile  = "file"
	EntryDir   = "dir"
	EntryLink  = "link"
	EntryOther = "other"
)

// FileInfo is one entry: its path relative to the workspace's root, its
// kind, size (regular files only), permission bits and modification time.
type FileInfo struct {
	Path  string    `json:"path"`
	Type  string    `json:"type"`
	Size  int64     `json:"size"`
	Mode  uint32    `json:"mode"`
	MTime time.Time `json:"mtime"`
	// SHA256 is the file's digest in hex, when fstat was asked for it.
	SHA256 string `json:"sha256,omitempty"`
}

// FStatArgs are an fstat op's arguments. Path "" or "." is the workspace's
// root.
type FStatArgs struct {
	Path   string `json:"path"`
	SHA256 bool   `json:"sha256,omitempty"`
}

// FListArgs are an fls op's arguments: one level of Path, or everything
// under it, at most Limit entries (zero is DefaultListLimit, at most
// MaxListLimit).
type FListArgs struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// The bounds of an fls listing.
const (
	DefaultListLimit = 1000
	MaxListLimit     = 10000
)

// FListAnswer is an fls op's output, in lexical order of path.
type FListAnswer struct {
	Entries   []FileInfo `json:"entries"`
	Truncated bool       `json:"truncated,omitempty"`
}

// GetArgs are a get op's arguments: Length bytes from Offset; Length zero is
// to the end of the file. The output is the bytes, raw.
type GetArgs struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Length int64  `json:"length,omitempty"`
}

// PutArgs are a put op's arguments. Size is exactly the bytes the input
// carries: a put that receives fewer or more is refused (size_mismatch) and
// leaves nothing. With Stage, Path is inside that staging folder.
type PutArgs struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Stage     string `json:"stage,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// PutAnswer is a put op's output.
type PutAnswer struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Usage
}

// Usage is how much of its quota a workspace holds, as the worker counts it.
// Estimate: the count's walk ran out of time, so Used is at least that much.
type Usage struct {
	UsedBytes  int64 `json:"used_bytes"`
	QuotaBytes int64 `json:"quota_bytes"`
	Estimate   bool  `json:"estimate,omitempty"`
}

// MkdirArgs are a mkdir op's arguments. Missing parents are made too; a
// folder already there is success.
type MkdirArgs struct {
	Path  string `json:"path"`
	Stage string `json:"stage,omitempty"`
}

// FRemoveArgs are an fremove op's arguments.
type FRemoveArgs struct {
	Path string `json:"path"`
}

// FRemoveAnswer is an fremove op's output: what was removed.
type FRemoveAnswer struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	Usage
}

// StageAnswer is a stage op's output: the staging folder's id.
type StageAnswer struct {
	Stage string `json:"stage"`
}

// CommitArgs are a commit op's arguments: the staging folder becomes Path.
// A Path already there is refused (exists) unless Overwrite, which merges
// the staged files into it file by file.
type CommitArgs struct {
	Stage     string `json:"stage"`
	Path      string `json:"path"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// DiscardArgs are a discard op's arguments; a stage already gone is success.
type DiscardArgs struct {
	Stage string `json:"stage"`
}

// CopyLocalArgs are a copy_local op's arguments, sent to the TARGET
// workspace: FromPath in workspace FromWorkspace (on the same worker) is
// copied to Path. A folder is refused before anything is written when it
// holds more than MaxFiles files or MaxBytes bytes; links in it are skipped
// and named in the answer.
type CopyLocalArgs struct {
	FromWorkspace string `json:"from_workspace"`
	FromPath      string `json:"from_path"`
	Path          string `json:"path"`
	Overwrite     bool   `json:"overwrite,omitempty"`
	MaxFiles      int    `json:"max_files"`
	MaxBytes      int64  `json:"max_bytes"`
	// Listed makes the copy of a folder exactly Files: the regular files
	// the core listed and judged, at paths relative to FromPath, each of
	// its listed size. Nothing else under FromPath is copied, and a listed
	// file now missing, not a regular file, a link, or of another size
	// refuses the copy with nothing placed. Without it the worker lists
	// the folder itself.
	Listed bool            `json:"listed,omitempty"`
	Files  []CopyLocalFile `json:"files,omitempty"`
	// MaxFileBytes, when positive, bounds each listed file.
	MaxFileBytes int64 `json:"max_file_bytes,omitempty"`
}

// CopyLocalFile is one file of a listed copy_local.
type CopyLocalFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// CopyAnswer is a copy_local op's output.
type CopyAnswer struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// Skipped names the links in a source folder that were not copied, at
	// most MaxSkippedNamed of them.
	Skipped []string `json:"skipped,omitempty"`
	Usage
}

// MaxSkippedNamed bounds the skipped links a copy names.
const MaxSkippedNamed = 100

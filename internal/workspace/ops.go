package workspace

import (
	"encoding/json"
	"time"
)

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
	// Root, on a personal worker, binds the workspace to the user's
	// directory of that name (RFC GW §4.5) instead of making one: the
	// workspace is that directory, never a copy.
	Root string `json:"root,omitempty"`
}

// Roots on a personal worker (RFC GW §4.5, P13). A root is a directory of the
// user's own machine the user named at the worker's start; the worker reports
// each by name as a label, never by path.
const (
	// RootLabelPrefix names a root among a worker's labels: "root:<name>".
	RootLabelPrefix = "root:"
	// ClassNotOnRoot: the op is refused on a root workspace (checkpoints,
	// restore, fork, templates): the directory is the user's own.
	ClassNotOnRoot = "not_on_root"
	// ClassRootBusy: another workspace is bound to the root.
	ClassRootBusy = "root_busy"
)

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
//
// Size PutSizeUnknown is an input whose length nobody knows ahead (a
// download whose response gave none), bounded by MaxBytes, which it
// requires (and which a known size may not carry). The worker admits
// MaxBytes against the quota before it reads a byte, refuses an input past
// it (size_mismatch) leaving nothing, and counts the bytes that came. A
// worker older than this form refuses it (invalid), never writes it.
type PutArgs struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	MaxBytes  int64  `json:"max_bytes,omitempty"`
	Stage     string `json:"stage,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// PutSizeUnknown is PutArgs.Size for an input of unknown length.
const PutSizeUnknown = -1

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
	// CheckpointBytes is what the workspace's checkpoints hold, counted
	// against the same quota beside UsedBytes (RFC GW P6).
	CheckpointBytes int64 `json:"checkpoint_bytes,omitempty"`
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

// The conversion ops (RFC GW D34): a workspace's file is converted on its
// worker, in the conversion child the worker supervises, and never crosses
// to the core. The source is opened as get opens one (a regular file, no
// link on its path) and must still hash to SHA256, the digest the core
// checked its quarantine with. The result goes into a workspace on the same
// worker (To.Workspace, through a stage committed in one rename), or
// streams back as the op's output (To.Workspace empty).
const (
	// OpConvertExtract extracts a file's text layer and embedded images,
	// as File op=extract does. Streamed back, its output is a tar
	// (ConvertTar*); into a workspace, its answer is ConvertExtractAnswer.
	OpConvertExtract = "convert_extract"
	// OpConvertUnpack unpacks a ZIP or RAR archive under the unpack bounds
	// of File op=unpack. Streamed back, its output is the unpack tar of
	// internal/files/unpack; into a workspace, its files are left in a
	// stage the core commits once it has judged each, and its answer is
	// ConvertUnpackAnswer.
	OpConvertUnpack = "convert_unpack"
	// OpConvertRenderHTML reads an HTML file into the Markdown File
	// op=render typesets; it always streams back, its output the
	// Markdown.
	OpConvertRenderHTML = "convert_render_html"
)

// The classes a conversion op adds.
const (
	// ClassHelperMissing: the worker's conversion child lacks the helper
	// program the conversion needs; the detail names it.
	ClassHelperMissing = "conversion_helper_missing"
	// ClassConversionCrashed: the file killed the conversion child. The
	// core quarantines it for that workspace.
	ClassConversionCrashed = "conversion_crashed"
	// ClassConversionUnsupported: nothing reads the file's type.
	ClassConversionUnsupported = "conversion_unsupported"
	// ClassConversionFailed: the file could not be read as its type, or
	// the conversion ran out of time.
	ClassConversionFailed = "conversion_failed"
	// ClassConversionBusy: the conversion child stayed busy.
	ClassConversionBusy = "conversion_busy"
	// ClassSourceChanged: the source no longer hashes to the digest the
	// core named.
	ClassSourceChanged = "source_changed"
	// ClassUnpackRefused: the archive is not one unpack reads, or is
	// refused whole (encrypted, multi-volume, damaged before any file);
	// the detail is the reason, worded to follow the archive's name.
	ClassUnpackRefused = "unpack_refused"
)

// ConvertTarget is where a conversion's result goes on the worker.
type ConvertTarget struct {
	// Workspace is the id of a workspace on this worker (the source's own
	// or another); empty streams the result back.
	Workspace string `json:"workspace,omitempty"`
	// Path is the folder the result is placed at in Workspace.
	Path      string `json:"path,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// ConvertSource is the file a conversion reads.
type ConvertSource struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	// MaxBytes, when positive, refuses a larger source.
	MaxBytes int64 `json:"max_bytes,omitempty"`
}

// ConvertExtractArgs are a convert_extract op's arguments: extract.Options
// as File op=extract passes them.
type ConvertExtractArgs struct {
	ConvertSource
	Images        bool  `json:"images,omitempty"`
	MaxRows       int   `json:"max_rows,omitempty"`
	InferHeadings bool  `json:"infer_headings,omitempty"`
	Pages         []int `json:"pages,omitempty"`
	MaxChars      int   `json:"max_chars,omitempty"`
	// MaxMarkdown bounds the Markdown, in bytes; past it the text is cut.
	MaxMarkdown int `json:"max_markdown"`
	// ImageMark, streamed back, is what an image's place in the Markdown is
	// written as: ImageMark, the image's number in the tar, and ImageMarkEnd.
	// The core replaces each with the reference it keeps the image at. A
	// random mark per call is one the file's own text cannot hold.
	ImageMark string        `json:"image_mark,omitempty"`
	To        ConvertTarget `json:"to"`
}

// ImageMarkEnd closes an image mark.
const ImageMarkEnd = "~"

// Target is where the result goes.
func (a *ConvertExtractArgs) Target() *ConvertTarget { return &a.To }

// ConvertExtractAnswer is a convert_extract op's answer.
type ConvertExtractAnswer struct {
	MediaType string `json:"media_type"`
	// Report is extract.Report as JSON; PageYields its PageYields, which
	// that JSON leaves out and OCR needs to pick its pages.
	Report     json.RawMessage `json:"report"`
	PageYields json.RawMessage `json:"page_yields,omitempty"`
	// Cut: the text stopped at MaxChars (Sampled) or MaxMarkdown.
	Sampled bool `json:"sampled,omitempty"`
	Cut     bool `json:"cut,omitempty"`
	// PageStarts is where each page's Markdown begins, in bytes.
	PageStarts map[int]int `json:"page_starts,omitempty"`
	// Into a workspace: the Markdown file's path and the images', relative
	// to the workspace root.
	Markdown string   `json:"markdown,omitempty"`
	Images   []string `json:"images,omitempty"`
	// MarkdownSHA256 is the Markdown file's digest as written, which an
	// md_splice of its pages names.
	MarkdownSHA256 string `json:"markdown_sha256,omitempty"`
	// NotKept names images not written, at most ten.
	NotKept []string `json:"not_kept,omitempty"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// The entries of a conversion's tar, streamed back. Each image of an
// extraction is an entry "image-<n>" with its metadata in PAX records
// (ConvertPAX*); the Markdown is "text.md"; the last entry is the answer.
const (
	ConvertTarImage  = "image-"
	ConvertTarText   = "text.md"
	ConvertTarAnswer = "answer.json"
	ConvertPAXName   = "LOOMCYCLE.name"
	ConvertPAXType   = "LOOMCYCLE.media_type"
	ConvertPAXAlt    = "LOOMCYCLE.alt"
	ConvertPAXPage   = "LOOMCYCLE.page"
	ConvertPAXSlide  = "LOOMCYCLE.slide"
)

// ConvertRenderHTMLArgs are a convert_render_html op's arguments.
type ConvertRenderHTMLArgs struct {
	ConvertSource
	MaxMarkdown int `json:"max_markdown"`
	// To is never set: the Markdown always streams back.
	To ConvertTarget `json:"-"`
}

// Target is where the result goes: always back to the core.
func (a *ConvertRenderHTMLArgs) Target() *ConvertTarget { return &a.To }

// ConvertUnpackArgs are a convert_unpack op's arguments: the bounds and
// choices of File op=unpack (unpack.Options).
type ConvertUnpackArgs struct {
	ConvertSource
	All        bool  `json:"all,omitempty"`
	DryRun     bool  `json:"dry_run,omitempty"`
	MaxListed  int   `json:"max_listed"`
	MaxWritten int   `json:"max_written"`
	MaxTotal   int64 `json:"max_total"`
	// Budget is what this unpack may inflate, at most MaxTotal.
	Budget  int64 `json:"budget"`
	PerFile int64 `json:"per_file"`
	// To, into a workspace, is where the stage is made. To.Path names the
	// folder the core will commit it at; the stage is committed by the
	// core, once it has judged every file in it.
	To ConvertTarget `json:"to"`
}

// Target is where the result goes.
func (a *ConvertUnpackArgs) Target() *ConvertTarget { return &a.To }

// ConvertUnpackAnswer is a convert_unpack op's answer into a workspace.
// Stage holds the files, at the Result's written paths; the core commits
// it, or discards it. A walk that stopped at a bound leaves no stage.
type ConvertUnpackAnswer struct {
	Stage  string `json:"stage,omitempty"`
	Format string `json:"format"`
	// Result is unpack.Result as JSON, its written refs paths relative to
	// the stage.
	Result json.RawMessage `json:"result"`
}

// The byte halves of OCR and speech-to-text on a workspace's file (RFC GW
// D34). A model call needs a provider key, which never reaches a worker, so
// the worker does the byte work in its conversion child and the core calls
// the model: convert_rasterize sends the pages OCR reads, convert_media the
// recording as the speech-to-text WAV, and md_splice writes the core's OCR
// text into Markdown that stays in the workspace. None of their arguments
// names a provider, a model or a credential.
const (
	// OpConvertRasterize cuts pages of a PDF in the workspace out as one
	// PDF, or renders one page to a PNG; for an image it sends the image.
	// Its output is the bytes, at most MaxUnit of them.
	OpConvertRasterize = "convert_rasterize"
	// OpConvertMedia converts a recording into the PCM WAV the
	// speech-to-text pass cuts. Its output is the WAV.
	OpConvertMedia = "convert_media"
	// OpMDSplice writes OCR text into a Markdown file in place of the pages
	// it read. Its input is MDSpliceInput as JSON; its answer
	// MDSpliceAnswer.
	OpMDSplice = "md_splice"
)

// ClassUnitTooLarge: what convert_rasterize made is over its MaxUnit; the
// core sends fewer pages at a time, or reports the page too large.
const ClassUnitTooLarge = "unit_too_large"

// ConvertRasterizeArgs are a convert_rasterize op's arguments.
type ConvertRasterizeArgs struct {
	ConvertSource
	// Pages are the PDF's pages, counted from 1; an image is page 1.
	Pages []int `json:"pages"`
	// Render renders the one page in Pages to a PNG; otherwise the pages
	// are cut out as a PDF.
	Render bool `json:"render,omitempty"`
	// Language is the OCR language hint the rasteriser may take (ISO 639-1
	// codes joined by +).
	Language string `json:"language,omitempty"`
	// MaxUnit bounds the output, in bytes.
	MaxUnit int64 `json:"max_unit"`
	// To is never set: the unit always streams back.
	To ConvertTarget `json:"-"`
}

// Target is where the result goes: always back to the core.
func (a *ConvertRasterizeArgs) Target() *ConvertTarget { return &a.To }

// ConvertMediaArgs are a convert_media op's arguments.
type ConvertMediaArgs struct {
	ConvertSource
	// Language is the spoken language hint the media helper may take.
	Language string `json:"language,omitempty"`
	// To is never set: the WAV always streams back.
	To ConvertTarget `json:"-"`
}

// Target is where the result goes: always back to the core.
func (a *ConvertMediaArgs) Target() *ConvertTarget { return &a.To }

// MDSpliceArgs are an md_splice op's arguments: the Markdown file, and the
// sha256 it had when its page offsets were reported, which it must still
// have.
type MDSpliceArgs struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// MDSpliceInput is an md_splice op's input.
type MDSpliceInput struct {
	// Header is the provenance comment written before the first page.
	Header string `json:"header"`
	// PageStarts is where each page's Markdown begins, in bytes; Image
	// takes the one page of an image's Markdown to start after its title.
	PageStarts map[int]int `json:"page_starts,omitempty"`
	Image      bool        `json:"image,omitempty"`
	// Pages is the OCR text of each page it replaces.
	Pages map[int]string `json:"pages"`
}

// MDSpliceAnswer is an md_splice op's answer.
type MDSpliceAnswer struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Usage
}

// The interpreter ops (RFC GW §5.7): a cell run in a kernel that stays
// alive in the workspace and keeps its variables, and the kernel's reset.
// The cell's code travels as the op's input, as a write's body does.
const (
	OpRunCell     = "run_cell"
	OpResetKernel = "reset_kernel"
)

// ClassKernelUnavailable: the worker cannot start the kernel asked for (no
// interpreter on its PATH, no driver); the detail says why.
const ClassKernelUnavailable = "kernel_unavailable"

// RunCellArgs are a run_cell op's input. TimeoutMS and GraceMS are decided
// by the core from the operator's settings; the worker refuses one that is
// not positive.
type RunCellArgs struct {
	Code      string `json:"code"`
	Kernel    string `json:"kernel"`
	TimeoutMS int64  `json:"timeout_ms"`
	GraceMS   int64  `json:"grace_ms"`
}

// RunCellAnswer is a run_cell op's output: the cell's output in the order it
// was written, its last value, its error, the pictures it left (paths in the
// workspace), and how it ended.
type RunCellAnswer struct {
	Kernel         string   `json:"kernel"`
	ExecutionCount int      `json:"execution_count"`
	Output         string   `json:"output"`
	Value          *string  `json:"value,omitempty"`
	Error          string   `json:"error,omitempty"`
	Pictures       []string `json:"pictures,omitempty"`
	Truncated      bool     `json:"truncated,omitempty"`
	Interrupted    bool     `json:"interrupted,omitempty"`
	TimedOut       bool     `json:"timed_out,omitempty"`
	// KernelKilled: the cell did not stop when interrupted, and the kernel
	// was killed; KernelDied: it exited by itself. Either way its variables
	// are gone.
	KernelKilled bool `json:"kernel_killed,omitempty"`
	KernelDied   bool `json:"kernel_died,omitempty"`
	// KernelRestarted: this cell ran in a new kernel because the one before
	// died (killed, crashed, its worker restarted), not because of a reset.
	KernelRestarted bool `json:"kernel_restarted,omitempty"`
}

// ResetKernelArgs are a reset_kernel op's arguments.
type ResetKernelArgs struct {
	Kernel string `json:"kernel"`
}

// ResetKernelAnswer is a reset_kernel op's output: Stopped is false when no
// kernel was running.
type ResetKernelAnswer struct {
	Kernel  string `json:"kernel"`
	Stopped bool   `json:"stopped"`
}

// The checkpoint ops (RFC GW §5.4, P6): a workspace's files recorded on its
// worker, outside the workspace, put back, listed and removed; and a second
// workspace opened from a checkpoint or from the present files. A
// checkpoint holds files only: never a kernel's variables or a running
// server. Each counts against its workspace's disk quota by its full size.
const (
	OpCheckpoint       = "checkpoint"
	OpRestore          = "restore"
	OpCheckpoints      = "checkpoints"
	OpDeleteCheckpoint = "delete_checkpoint"
	// OpFork is sent for the NEW workspace's id: its directory is made as a
	// copy of ForkArgs.From (or of one of its checkpoints) on this worker.
	OpFork = "fork"
)

// The mechanisms a worker records a checkpoint with, the cheapest its disk
// offers (RFC GW §15 spike F). Never hard links: a copy made of them shares
// its files with the original, and an append or a chmod changes both.
const (
	// MechanismClone is a whole-directory clone (APFS clonefile).
	MechanismClone = "clone"
	// MechanismReflink copies each file by sharing its blocks (FICLONE).
	MechanismReflink = "reflink"
	// MechanismCopy copies each file's bytes.
	MechanismCopy = "copy"
)

// CheckpointLabelPrefix is the worker label naming its mechanism:
// "checkpoint:<mechanism>".
const CheckpointLabelPrefix = "checkpoint:"

// DefaultMaxCheckpoints bounds a workspace's checkpoints when the core names
// no bound: config workspace.checkpoint.max_per_workspace's default.
const DefaultMaxCheckpoints = 8

// MaxCheckpointLabel bounds a checkpoint's label, in characters.
const MaxCheckpointLabel = 200

// The classes the checkpoint ops add.
const (
	// ClassCheckpointLimit: the workspace holds as many checkpoints as it
	// may.
	ClassCheckpointLimit = "checkpoint_limit"
	// ClassCheckpointNotFound: the workspace has no checkpoint of that id.
	ClassCheckpointNotFound = "checkpoint_not_found"
)

// CheckpointArgs are a checkpoint op's arguments.
type CheckpointArgs struct {
	Label string `json:"label,omitempty"`
	// Max is the checkpoints the workspace may hold; zero is
	// DefaultMaxCheckpoints.
	Max int `json:"max,omitempty"`
}

// CheckpointRef names one checkpoint of the op's workspace: restore's and
// delete_checkpoint's arguments.
type CheckpointRef struct {
	Checkpoint string `json:"checkpoint"`
}

// CheckpointInfo is one checkpoint as the worker records it.
type CheckpointInfo struct {
	ID        string    `json:"id"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Bytes is what it counts against the quota: its files' full size,
	// whatever the mechanism shares with the workspace now.
	Bytes     int64  `json:"bytes"`
	Files     int    `json:"files"`
	Mechanism string `json:"mechanism"`
	// Skipped names what was not copied (a FIFO, a socket, a device), at
	// most MaxSkippedNamed.
	Skipped []string `json:"skipped,omitempty"`
}

// CheckpointAnswer is a checkpoint op's output.
type CheckpointAnswer struct {
	CheckpointInfo
	Usage Usage `json:"usage"`
}

// CheckpointsAnswer is a checkpoints op's output, newest first.
type CheckpointsAnswer struct {
	Checkpoints []CheckpointInfo `json:"checkpoints"`
	Usage       Usage            `json:"usage"`
}

// RestoreAnswer is a restore op's output. KernelsStopped and SessionStopped
// say what the restore ended: a restore puts back files, never a process.
type RestoreAnswer struct {
	Checkpoint     string `json:"checkpoint"`
	KernelsStopped bool   `json:"kernels_stopped,omitempty"`
	SessionStopped bool   `json:"session_stopped"`
	Usage          Usage  `json:"usage"`
}

// DeleteCheckpointAnswer is a delete_checkpoint op's output; Deleted is
// false when no such checkpoint was there.
type DeleteCheckpointAnswer struct {
	Deleted bool  `json:"deleted"`
	Usage   Usage `json:"usage"`
}

// ForkArgs are a fork op's arguments, sent for the new workspace's id.
type ForkArgs struct {
	// From is the id of the workspace forked, on this worker.
	From string `json:"from"`
	// Checkpoint, when set, is the checkpoint of From to start from;
	// empty is From's present files.
	Checkpoint string `json:"checkpoint,omitempty"`
	// DiskQuotaBytes is the new workspace's quota, as open's; zero is the
	// worker's default. The copy must fit it.
	DiskQuotaBytes int64 `json:"disk_quota_bytes,omitempty"`
}

// ForkAnswer is a fork op's output. Rewritten names the files of a Python
// environment whose absolute paths were moved to the fork's directory (the
// host engine only: a container session mounts every workspace at one
// path, so nothing needs it).
type ForkAnswer struct {
	Bytes     int64    `json:"bytes"`
	Files     int      `json:"files"`
	Mechanism string   `json:"mechanism"`
	Skipped   []string `json:"skipped,omitempty"`
	Rewritten []string `json:"rewritten,omitempty"`
	Usage     Usage    `json:"usage"`
}

// The template ops (RFC GW §5.5, P6). A template is a checkpoint packed as
// one archive (tar, gzip) the core keeps by digest; a worker that opens one
// receives the archive once, unpacks it into a cache of its own outside
// every workspace, and makes each workspace opened from it as a copy of
// that tree.
const (
	// OpTemplatePack streams a checkpoint of the op's workspace out as a
	// template archive (TemplatePackArgs).
	OpTemplatePack = "template_pack"
	// OpTemplatePut streams an archive in and unpacks it into the worker's
	// cache under its digest (TemplatePutArgs). Sent with the id of the
	// workspace about to be opened from it, which need not exist yet.
	OpTemplatePut = "template_put"
	// OpTemplateOpen is sent for the NEW workspace's id: its directory is
	// made as a copy of the cached template (TemplateOpenArgs), answered as
	// a fork is (ForkAnswer). A worker that does not hold the template
	// answers ClassTemplateMissing.
	OpTemplateOpen = "template_open"
)

// ClassTemplateMissing: the worker does not hold the template; the core
// sends it with OpTemplatePut and opens again.
const ClassTemplateMissing = "template_missing"

// TemplatePackArgs are a template_pack op's arguments.
type TemplatePackArgs struct {
	// Checkpoint is the checkpoint of the op's workspace to pack.
	Checkpoint string `json:"checkpoint"`
}

// TemplatePutArgs are a template_put op's arguments: the archive's SHA-256
// (hex) and size, and MaxBytes, what its unpacked files may hold.
type TemplatePutArgs struct {
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	MaxBytes int64  `json:"max_bytes"`
}

// TemplateOpenArgs are a template_open op's arguments.
type TemplateOpenArgs struct {
	Digest string `json:"digest"`
	// DiskQuotaBytes is the new workspace's quota, as open's; the copy must
	// fit it.
	DiskQuotaBytes int64 `json:"disk_quota_bytes,omitempty"`
}

// ValidTemplateDigest reports whether d is a SHA-256 in lowercase hex: what
// names a template in a worker's cache, so it is never a path.
func ValidTemplateDigest(d string) bool {
	if len(d) != 64 {
		return false
	}
	for i := 0; i < len(d); i++ {
		if c := d[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// OpPark ends what runs in a workspace (its servers, its interpreters, its
// session) and keeps its directory: a durable workspace no run holds (RFC
// GW P12 plan §3.3). It takes no arguments and is idempotent.
const OpPark = "park"

package workspace

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

package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/NikitaForGit/LazyTerra/internal/deps"
	"github.com/NikitaForGit/LazyTerra/internal/discovery"
	"github.com/NikitaForGit/LazyTerra/internal/identity"
	"github.com/NikitaForGit/LazyTerra/internal/runner"
	"github.com/NikitaForGit/LazyTerra/internal/version"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------------------------------------------------------------------------
// Pane identifiers
//
//	[1] Modules   [2] Dependencies   [3] State Browser   [4] Info   [5] File View
//	Command Log is unnumbered — toggled with @
// ---------------------------------------------------------------------------

type focusPane int

const (
	paneModules  focusPane = iota // [1]
	paneDeps                      // [2]
	paneState                     // [3]
	paneInfo                      // [4]
	paneFileView                  // [5]
	paneCmdLog                    // not numbered, toggled with @
)

func paneName(p focusPane) string {
	switch p {
	case paneModules:
		return "Modules"
	case paneDeps:
		return "Dependencies"
	case paneState:
		return "State Browser"
	case paneInfo:
		return "Info"
	case paneFileView:
		return "File View"
	case paneCmdLog:
		return "Command Log"
	}
	return ""
}

// paneNumber returns the display number for numbered panes, 0 for unnumbered.
func paneNumber(p focusPane) int {
	switch p {
	case paneModules:
		return 1
	case paneDeps:
		return 2
	case paneState:
		return 3
	case paneInfo:
		return 4
	case paneFileView:
		return 5
	}
	return 0
}

// ---------------------------------------------------------------------------
// Command log line — stores both structured entries and raw output
// ---------------------------------------------------------------------------

type cmdLogLine struct {
	Text    string
	IsError bool
}

// tfLogLevels is the ordered list of TF_LOG levels displayed in the picker.
// "" means OFF (no TF_LOG env var set).
var tfLogLevels = []string{"", "ERROR", "WARN", "INFO", "DEBUG", "TRACE"}

// ---------------------------------------------------------------------------
// Tree node (collapsible directory tree)
// ---------------------------------------------------------------------------

type treeNode struct {
	Name      string
	FullPath  string
	Depth     int
	IsDir     bool
	Collapsed bool
	ModuleIdx int // -1 for dirs
	Children  []int
	Parent    int // -1 for roots
}

func buildTree(modules []discovery.Module) []treeNode {
	type tmpNode struct {
		name      string
		children  map[string]*tmpNode
		order     []string
		moduleIdx int
		fullPath  string
	}

	root := &tmpNode{children: make(map[string]*tmpNode), moduleIdx: -1}

	for i, mod := range modules {
		parts := strings.Split(mod.Dir, "/")
		cur := root
		for pi, p := range parts {
			if _, ok := cur.children[p]; !ok {
				child := &tmpNode{
					name:      p,
					children:  make(map[string]*tmpNode),
					moduleIdx: -1,
				}
				cur.children[p] = child
				cur.order = append(cur.order, p)
			}
			cur = cur.children[p]
			if pi == len(parts)-1 {
				cur.moduleIdx = i
				cur.fullPath = mod.Dir
			}
		}
	}

	var nodes []treeNode
	var flatten func(n *tmpNode, depth int, parentIdx int)
	flatten = func(n *tmpNode, depth int, parentIdx int) {
		for _, key := range n.order {
			child := n.children[key]
			idx := len(nodes)
			isDir := len(child.children) > 0
			node := treeNode{
				Name:      child.name,
				FullPath:  child.fullPath,
				Depth:     depth,
				IsDir:     isDir,
				Collapsed: false,
				ModuleIdx: child.moduleIdx,
				Parent:    parentIdx,
			}
			nodes = append(nodes, node)
			if parentIdx >= 0 {
				nodes[parentIdx].Children = append(nodes[parentIdx].Children, idx)
			}
			if isDir {
				flatten(child, depth+1, idx)
			}
		}
	}
	flatten(root, 0, -1)
	return nodes
}

func visibleNodes(nodes []treeNode) []int {
	hidden := make(map[int]bool)
	var result []int
	for i, n := range nodes {
		if hidden[i] {
			continue
		}
		result = append(result, i)
		if n.IsDir && n.Collapsed {
			var markHidden func(idx int)
			markHidden = func(idx int) {
				for _, ci := range nodes[idx].Children {
					hidden[ci] = true
					markHidden(ci)
				}
			}
			markHidden(i)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// HCL syntax highlighting
// ---------------------------------------------------------------------------

var (
	hclKeywordRe = regexp.MustCompile(`\b(terraform|terragrunt|dependency|dependencies|include|locals|inputs|remote_state|generate|prevent_destroy|skip)\b`)
	hclBlockRe   = regexp.MustCompile(`\b(source|config_path|path|backend|bucket|key|region|encrypt|dynamodb_table)\b`)
	hclFuncRe    = regexp.MustCompile(`\b(find_in_parent_folders|path_relative_to_include|path_relative_from_include|get_terragrunt_dir|get_parent_terragrunt_dir|get_env|read_terragrunt_config|local|dependency)\s*\(`)
	hclStringRe  = regexp.MustCompile(`"[^"]*"`)
	hclBoolRe    = regexp.MustCompile(`\b(true|false|null)\b`)
	hclNumberRe  = regexp.MustCompile(`\b\d+\b`)
	synKeyword   = lipgloss.NewStyle().Foreground(lipgloss.Color("#CBA6F7"))              // purple
	synBlock     = lipgloss.NewStyle().Foreground(lipgloss.Color("#89B4FA"))              // blue
	synFunc      = lipgloss.NewStyle().Foreground(lipgloss.Color("#F9E2AF"))              // yellow
	synString    = lipgloss.NewStyle().Foreground(lipgloss.Color("#A6E3A1"))              // green
	synComment   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Italic(true) // dim
	synBool      = lipgloss.NewStyle().Foreground(lipgloss.Color("#FAB387"))              // orange
	synNumber    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FAB387"))              // orange
)

// highlightHCL applies syntax coloring to a single line of HCL.
// We use a token-based approach to avoid corrupting ANSI sequences.
func highlightHCL(line string) string {
	if strings.TrimSpace(line) == "" {
		return line
	}

	// If the whole line is a comment, style it all at once.
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
		return synComment.Render(line)
	}

	// Token-based highlighting to avoid overlapping ANSI issues.
	// We identify all tokens with their positions, sort them, and then
	// build the output by styling each segment appropriately.

	type token struct {
		start int
		end   int
		style lipgloss.Style
	}

	var tokens []token

	// Find all string literals first (highest priority).
	for _, loc := range hclStringRe.FindAllStringIndex(line, -1) {
		tokens = append(tokens, token{loc[0], loc[1], synString})
	}

	// Helper to check if a position is inside any string
	inString := func(pos int) bool {
		for _, t := range tokens {
			if pos >= t.start && pos < t.end {
				return true
			}
		}
		return false
	}

	// Find keywords (skip if inside strings)
	for _, loc := range hclKeywordRe.FindAllStringIndex(line, -1) {
		if !inString(loc[0]) {
			tokens = append(tokens, token{loc[0], loc[1], synKeyword})
		}
	}

	// Find block names (skip if inside strings)
	for _, loc := range hclBlockRe.FindAllStringIndex(line, -1) {
		if !inString(loc[0]) {
			tokens = append(tokens, token{loc[0], loc[1], synBlock})
		}
	}

	// Find booleans (skip if inside strings)
	for _, loc := range hclBoolRe.FindAllStringIndex(line, -1) {
		if !inString(loc[0]) {
			tokens = append(tokens, token{loc[0], loc[1], synBool})
		}
	}

	// Find numbers (skip if inside strings)
	for _, loc := range hclNumberRe.FindAllStringIndex(line, -1) {
		if !inString(loc[0]) {
			tokens = append(tokens, token{loc[0], loc[1], synNumber})
		}
	}

	// Find functions (skip if inside strings) - color just the function name, not the paren
	for _, loc := range hclFuncRe.FindAllStringIndex(line, -1) {
		if !inString(loc[0]) {
			// loc[1]-1 to exclude the opening parenthesis
			tokens = append(tokens, token{loc[0], loc[1] - 1, synFunc})
		}
	}

	if len(tokens) == 0 {
		return line
	}

	// Sort tokens by start position
	sort.Slice(tokens, func(i, j int) bool {
		return tokens[i].start < tokens[j].start
	})

	// Build output by processing each segment
	var out strings.Builder
	pos := 0
	for _, t := range tokens {
		// Skip overlapping tokens (string tokens take priority)
		if t.start < pos {
			continue
		}
		// Add unstyled text before this token
		if t.start > pos {
			out.WriteString(line[pos:t.start])
		}
		// Add styled token
		out.WriteString(t.style.Render(line[t.start:t.end]))
		pos = t.end
	}
	// Add remaining unstyled text
	if pos < len(line) {
		out.WriteString(line[pos:])
	}

	return out.String()
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type Model struct {
	// Data
	modules []discovery.Module
	rootDir string // Absolute path to the root directory
	tree    []treeNode
	visible []int

	selected map[int]bool
	cursor   int

	// State Browser
	stateResources []string // resource addresses from `terragrunt state list`
	stateCursor    int
	stateModule    string // relative dir of the module whose state is loaded
	stateAbsDir    string // absolute dir for running state commands
	stateTainted   map[string]bool
	stateExpanded  bool // toggle: expand state browser to full left-column width

	// Runner
	runner *runner.Runner

	// Command log (full output lines + headers)
	cmdLogLines  []cmdLogLine
	cmdLogScroll int
	lastCmdName  string

	// File view
	fileLines  []string
	fileScroll int
	fileCursor int // Current line position (0-indexed)
	filePath   string
	yankStart  int
	yankEnd    int
	yankedText string

	// Dependencies
	currentDeps []deps.Dependency
	depsCursor  int

	// Versions
	versionInfo version.Info

	// UI state
	focus       focusPane
	width       int
	height      int
	showHelp    bool
	helpScroll  int
	showConfirm bool
	confirmCmd  runner.Command
	searchMode  bool
	searchInput textinput.Model
	filterText  string

	// Modules view mode: false = tree view, true = flat path view
	modulesFlat bool

	flash string

	// Header bar — execution context from HCL config
	headerCtx identity.HeaderContext

	// Help overlay state
	helpLineCount int // total number of content lines (for scroll clamping)

	// TF_LOG level picker
	tfLogLevel         string // current TF_LOG level ("" = OFF/unset)
	showLogLevelPicker bool
	logLevelCursor     int

	// Force unlock prompt
	showLockIDPrompt bool
	lockIDInput      textinput.Model
	unlockModule     string // relative dir of the module to unlock

	// App version (set via ldflags at build time)
	appVersion string
}

func NewModel(modules []discovery.Module, rootDir string, appVersion string) Model {
	ti := textinput.New()
	ti.Placeholder = "search..."
	ti.CharLimit = 128

	lockInput := textinput.New()
	lockInput.Placeholder = "enter lock ID..."
	lockInput.CharLimit = 256

	tree := buildTree(modules)
	vis := visibleNodes(tree)

	m := Model{
		modules:      modules,
		rootDir:      rootDir,
		tree:         tree,
		visible:      vis,
		selected:     make(map[int]bool),
		stateTainted: make(map[string]bool),
		runner:       runner.New(),
		focus:        paneModules,
		yankStart:    -1,
		yankEnd:      -1,
		appVersion:   appVersion,
		lockIDInput:  lockInput,
	}
	m.searchInput = ti

	m.versionInfo = version.Detect()
	m.loadFileForCursor()
	m.loadDepsForCursor()

	return m
}

func (m *Model) rebuildVisible() {
	if m.modulesFlat {
		// Flat view: show only module nodes (no directories)
		m.visible = nil
		lower := strings.ToLower(m.filterText)
		for i, n := range m.tree {
			if n.ModuleIdx >= 0 {
				if m.filterText == "" || strings.Contains(strings.ToLower(n.FullPath), lower) {
					m.visible = append(m.visible, i)
				}
			}
		}
	} else if m.filterText == "" {
		m.visible = visibleNodes(m.tree)
	} else {
		lower := strings.ToLower(m.filterText)
		m.visible = nil
		for i, n := range m.tree {
			if n.ModuleIdx >= 0 && strings.Contains(strings.ToLower(n.FullPath), lower) {
				m.visible = append(m.visible, i)
			}
		}
	}
}

func (m *Model) loadFileForCursor() {
	m.fileLines = nil
	m.fileScroll = 0
	m.fileCursor = 0
	m.filePath = ""
	m.yankStart = -1
	m.yankEnd = -1

	if len(m.visible) == 0 || m.cursor >= len(m.visible) {
		return
	}
	treeIdx := m.visible[m.cursor]
	node := m.tree[treeIdx]
	if node.ModuleIdx < 0 {
		return
	}
	mod := m.modules[node.ModuleIdx]
	hclPath := filepath.Join(mod.AbsDir, "terragrunt.hcl")
	data, err := os.ReadFile(hclPath)
	if err != nil {
		m.fileLines = []string{fmt.Sprintf("Error: %v", err)}
		return
	}
	m.filePath = filepath.Join(mod.Dir, "terragrunt.hcl")
	m.fileLines = strings.Split(string(data), "\n")

	// Resolve header context from HCL config and root include.
	m.headerCtx = identity.ResolveContext(mod.AbsDir, mod.Dir, string(data))
}

func (m *Model) loadDepsForCursor() {
	m.currentDeps = nil
	m.depsCursor = 0

	if len(m.visible) == 0 || m.cursor >= len(m.visible) {
		return
	}
	treeIdx := m.visible[m.cursor]
	node := m.tree[treeIdx]
	if node.ModuleIdx < 0 {
		return
	}
	mod := m.modules[node.ModuleIdx]
	hclPath := filepath.Join(mod.AbsDir, "terragrunt.hcl")
	parsed, err := deps.Parse(hclPath)
	if err != nil {
		return
	}
	m.currentDeps = parsed
}

// refreshHeaderForFocus updates the header bar context based on the currently
// focused pane. When the state browser is focused and has state loaded, the
// header reflects the state module's context. Otherwise it reflects the module
// under the cursor in the modules pane.
func (m *Model) refreshHeaderForFocus() {
	if m.focus == paneState && m.stateAbsDir != "" {
		hclPath := filepath.Join(m.stateAbsDir, "terragrunt.hcl")
		if data, err := os.ReadFile(hclPath); err == nil {
			m.headerCtx = identity.ResolveContext(m.stateAbsDir, m.stateModule, string(data))
		}
	} else {
		// Restore header from the module under cursor.
		_, absDir := m.cursorModuleDir()
		if absDir != "" {
			hclPath := filepath.Join(absDir, "terragrunt.hcl")
			if data, err := os.ReadFile(hclPath); err == nil {
				relDir, _ := m.cursorModuleDir()
				m.headerCtx = identity.ResolveContext(absDir, relDir, string(data))
			}
		}
	}
}

// cursorModuleDir returns (relDir, absDir) for the module under cursor, or empty.
func (m Model) cursorModuleDir() (string, string) {
	if len(m.visible) == 0 || m.cursor >= len(m.visible) {
		return "", ""
	}
	treeIdx := m.visible[m.cursor]
	node := m.tree[treeIdx]
	if node.ModuleIdx < 0 {
		return "", ""
	}
	mod := m.modules[node.ModuleIdx]
	return mod.Dir, mod.AbsDir
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

type (
	outputMsg runner.OutputLine
	resultMsg runner.Result
)

// stateListMsg delivers the parsed state list results to the UI.
type stateListMsg struct {
	module    string
	absDir    string
	resources []string
	err       error
}

// flashDismissMsg is sent after the flash timer expires.
type flashDismissMsg struct{}

// editorFinishedMsg is sent when $EDITOR exits, so we can reload the file view.
type editorFinishedMsg struct{ err error }

// flashDismissCmd returns a command that sends flashDismissMsg after a delay.
func flashDismissCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg {
		return flashDismissMsg{}
	})
}

func tickOutput(r *runner.Runner) tea.Cmd {
	return func() tea.Msg {
		line := <-r.OutputChan()
		return outputMsg(line)
	}
}

func tickResult(r *runner.Runner) tea.Cmd {
	return func() tea.Msg {
		res := <-r.ResultChan()
		return resultMsg(res)
	}
}

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		tickOutput(m.runner),
		tickResult(m.runner),
	)
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case outputMsg:
		line := runner.OutputLine(msg)
		// Append every output line to the command log.
		m.cmdLogLines = append(m.cmdLogLines, cmdLogLine{
			Text:    line.Text,
			IsError: line.IsError,
		})
		// Auto-scroll to bottom.
		m.cmdLogScroll = maxScroll(len(m.cmdLogLines), m.cmdLogViewHeight())
		return m, tickOutput(m.runner)

	case resultMsg:
		return m, tickResult(m.runner)

	case stateListMsg:
		if msg.err != nil {
			m.cmdLogLines = append(m.cmdLogLines, cmdLogLine{Text: fmt.Sprintf("Error: %v", msg.err), IsError: true})
			m.cmdLogScroll = maxScroll(len(m.cmdLogLines), m.cmdLogViewHeight())
			m.flash = fmt.Sprintf("State list failed: %v", msg.err)
			return m, flashDismissCmd()
		}
		// Log results to command log.
		m.cmdLogLines = append(m.cmdLogLines, cmdLogLine{Text: fmt.Sprintf("Found %d resources", len(msg.resources))})
		for _, r := range msg.resources {
			m.cmdLogLines = append(m.cmdLogLines, cmdLogLine{Text: "  " + r})
		}
		m.cmdLogScroll = maxScroll(len(m.cmdLogLines), m.cmdLogViewHeight())
		m.stateResources = msg.resources
		m.stateCursor = 0
		m.stateModule = msg.module
		m.stateAbsDir = msg.absDir
		m.stateTainted = make(map[string]bool)
		m.focus = paneState
		m.flash = ""
		// Update header bar to reflect the state module's context.
		hclPath := filepath.Join(msg.absDir, "terragrunt.hcl")
		if data, err := os.ReadFile(hclPath); err == nil {
			m.headerCtx = identity.ResolveContext(msg.absDir, msg.module, string(data))
		}
		return m, nil

	case flashDismissMsg:
		m.flash = ""
		return m, nil

	case editorFinishedMsg:
		// Reload file view after editor exits.
		m.loadFileForCursor()
		if msg.err != nil {
			m.flash = fmt.Sprintf("Editor exited with error: %v", msg.err)
			return m, flashDismissCmd()
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.showConfirm {
		return m.handleConfirmKey(msg)
	}
	if m.showLogLevelPicker {
		return m.handleLogLevelKey(msg)
	}
	if m.showLockIDPrompt {
		return m.handleLockIDKey(msg)
	}
	if m.searchMode {
		return m.handleSearchKey(msg)
	}
	if m.showHelp {
		return m.handleHelpKey(msg)
	}

	key := msg.String()

	// Global keys.
	switch key {
	case "q", "ctrl+c":
		m.runner.Cancel()
		return m, tea.Quit

	case "Q":
		// Kill all running ops + quit
		m.runner.Cancel()
		return m, tea.Quit

	case "?":
		m.showHelp = true
		m.helpScroll = 0
		// Pre-compute helpLineCount so scrolling works on the first keypress.
		m.helpView()
		return m, nil

	case "T":
		m.showLogLevelPicker = true
		// Pre-select current log level in the picker.
		m.logLevelCursor = 0
		for i, lvl := range tfLogLevels {
			if lvl == m.tfLogLevel {
				m.logLevelCursor = i
				break
			}
		}
		return m, nil

	case "tab":
		// Cycle forward through numbered panes (skip cmdLog).
		switch m.focus {
		case paneModules:
			m.focus = paneDeps
		case paneDeps:
			m.focus = paneState
		case paneState:
			m.focus = paneInfo
		case paneInfo:
			m.focus = paneFileView
		case paneFileView:
			m.focus = paneModules
		case paneCmdLog:
			m.focus = paneModules
		}
		m.refreshHeaderForFocus()
		return m, nil

	case "shift+tab":
		// Cycle backward through numbered panes.
		switch m.focus {
		case paneModules:
			m.focus = paneFileView
		case paneDeps:
			m.focus = paneModules
		case paneState:
			m.focus = paneDeps
		case paneInfo:
			m.focus = paneState
		case paneFileView:
			m.focus = paneInfo
		case paneCmdLog:
			m.focus = paneModules
		}
		m.refreshHeaderForFocus()
		return m, nil

	case "1":
		if m.focus == paneModules {
			// Capture the current node identity before toggling.
			var targetPath string
			targetModIdx := -1
			if m.cursor < len(m.visible) {
				idx := m.visible[m.cursor]
				if idx < len(m.tree) {
					targetPath = m.tree[idx].FullPath
					targetModIdx = m.tree[idx].ModuleIdx
				}
			}
			// Toggle between tree view and flat path view
			m.modulesFlat = !m.modulesFlat
			m.rebuildVisible()
			// Restore cursor to the same module/dir in the new view.
			restored := false
			if targetPath != "" {
				for vi, ti := range m.visible {
					if ti < len(m.tree) {
						n := m.tree[ti]
						if n.FullPath == targetPath || (targetModIdx >= 0 && n.ModuleIdx == targetModIdx) {
							m.cursor = vi
							restored = true
							break
						}
					}
				}
			}
			if !restored {
				if m.cursor >= len(m.visible) {
					m.cursor = max(0, len(m.visible)-1)
				}
			}
		}
		m.focus = paneModules
		m.refreshHeaderForFocus()
		return m, nil
	case "2":
		m.focus = paneDeps
		m.refreshHeaderForFocus()
		return m, nil
	case "3":
		m.focus = paneState
		m.refreshHeaderForFocus()
		return m, nil
	case "4":
		m.focus = paneInfo
		m.refreshHeaderForFocus()
		return m, nil
	case "5":
		m.focus = paneFileView
		m.refreshHeaderForFocus()
		return m, nil
	case "@":
		m.focus = paneCmdLog
		m.refreshHeaderForFocus()
		return m, nil

	case "/":
		if m.focus == paneModules {
			m.searchMode = true
			m.searchInput.Focus()
			return m, textinput.Blink
		}

	case "esc":
		if m.focus == paneFileView && m.yankStart >= 0 {
			m.yankStart = -1
			m.yankEnd = -1
			return m, nil
		}
		if m.runner.IsRunning() {
			m.runner.Cancel()
			m.flash = "Cancelled running commands"
			return m, flashDismissCmd()
		}
		return m, nil
	}

	switch m.focus {
	case paneModules:
		return m.handleModuleKey(key)
	case paneState:
		return m.handleStateKey(key)
	case paneFileView:
		return m.handleFileViewKey(key)
	case paneCmdLog:
		return m.handleCmdLogKey(key)
	case paneDeps:
		return m.handleDepsKey(key)
	case paneInfo:
		// static
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// [1] Modules pane keys
// ---------------------------------------------------------------------------

func (m Model) handleModuleKey(key string) (tea.Model, tea.Cmd) {
	visCount := len(m.visible)
	if visCount == 0 {
		return m, nil
	}

	prevCursor := m.cursor

	switch key {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < visCount-1 {
			m.cursor++
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = visCount - 1
	case "ctrl+d":
		half := m.modulesViewHeight() / 2
		m.cursor += half
		if m.cursor >= visCount {
			m.cursor = visCount - 1
		}
	case "ctrl+u":
		half := m.modulesViewHeight() / 2
		m.cursor -= half
		if m.cursor < 0 {
			m.cursor = 0
		}

	case " ":
		treeIdx := m.visible[m.cursor]
		node := &m.tree[treeIdx]
		if node.ModuleIdx >= 0 {
			if m.selected[node.ModuleIdx] {
				delete(m.selected, node.ModuleIdx)
				// Clear the status indicator when unselecting
				m.runner.ClearStatus(m.modules[node.ModuleIdx].Dir)
			} else {
				m.selected[node.ModuleIdx] = true
			}
		} else {
			descendants := m.collectDescendantModules(treeIdx)
			allSelected := true
			for _, mi := range descendants {
				if !m.selected[mi] {
					allSelected = false
					break
				}
			}
			for _, mi := range descendants {
				if allSelected {
					delete(m.selected, mi)
					// Clear the status indicator when unselecting
					m.runner.ClearStatus(m.modules[mi].Dir)
				} else {
					m.selected[mi] = true
				}
			}
		}
		return m, nil

	case "enter":
		if m.cursor < visCount {
			treeIdx := m.visible[m.cursor]
			node := &m.tree[treeIdx]
			if node.IsDir {
				node.Collapsed = !node.Collapsed
				m.rebuildVisible()
				if m.cursor >= len(m.visible) {
					m.cursor = len(m.visible) - 1
				}
			} else {
				m.focus = paneFileView
				m.fileScroll = 0
			}
		}
		return m, nil

	case "h":
		// Collapse: if on an expanded dir, collapse it. If on a leaf or
		// collapsed dir, jump to parent dir and collapse it in one step.
		if m.cursor < visCount {
			treeIdx := m.visible[m.cursor]
			node := &m.tree[treeIdx]
			if node.IsDir && !node.Collapsed {
				// On an expanded dir — just collapse.
				node.Collapsed = true
				m.rebuildVisible()
			} else if node.Depth > 0 {
				// On a leaf or collapsed dir — find parent, collapse it, move cursor.
				for i := m.cursor - 1; i >= 0; i-- {
					pIdx := m.visible[i]
					pNode := &m.tree[pIdx]
					if pNode.IsDir && pNode.Depth < node.Depth {
						pNode.Collapsed = true
						m.cursor = i
						m.rebuildVisible()
						break
					}
				}
			}
		}
		return m, nil

	case "l":
		// Expand directory node.
		if m.cursor < visCount {
			treeIdx := m.visible[m.cursor]
			node := &m.tree[treeIdx]
			if node.IsDir && node.Collapsed {
				node.Collapsed = false
				m.rebuildVisible()
			}
		}
		return m, nil

	// Operations
	case "p":
		// Plan single cursor module (regardless of selection).
		if m.runner.IsRunning() {
			m.flash = "Command already running — press Esc to cancel first"
			return m, flashDismissCmd()
		}
		relDir, absDir := m.cursorModuleDir()
		if relDir == "" {
			m.flash = "No module under cursor"
			return m, flashDismissCmd()
		}
		m.flash = ""
		m.lastCmdName = runner.CmdPlan.Name
		m.runner.Run(runner.CmdPlan, []string{relDir}, map[string]string{relDir: absDir})
		return m, nil
	case "P":
		if len(m.selected) < 2 {
			m.flash = "Select 2+ modules with Space first"
			return m, flashDismissCmd()
		}
		return m.runAllOnSelected(runner.CmdPlan)
	case "a":
		// Apply single — only when exactly 1 (or 0 = cursor) module selected
		if len(m.selected) > 1 {
			m.flash = "Multiple modules selected — use 'A' for apply-all"
			return m, flashDismissCmd()
		}
		return m.confirmAndRun(runner.CmdApply)
	case "A":
		// Apply-all — only when more than 1 module selected
		if len(m.selected) <= 1 {
			m.flash = "Select 2+ modules with Space first — use 'a' for single"
			return m, flashDismissCmd()
		}
		return m.confirmAndRun(runner.CmdApply)
	case "d":
		return m.confirmAndRun(runner.CmdDestroy)
	case "D":
		return m.confirmAndRun(runner.CmdDestroy) // destroy-all uses same confirm gate
	case "i":
		return m.runOnSelected(runner.CmdInit)
	case "I":
		return m.runOnSelected(runner.CmdInitReconfigure)
	case "v":
		return m.runOnSelected(runner.CmdValidate)
	case "o":
		return m.runOnSelected(runner.CmdOutput)
	case "r":
		return m.runOnSelected(runner.CmdPlan)

	case "e":
		return m.openInEditor(false)
	case "E":
		return m.openInEditor(true)

	case "y":
		relDir, _ := m.cursorModuleDir()
		if relDir != "" {
			m.yankedText = relDir
			m.flash = fmt.Sprintf("Copied: %s", relDir)
			return m, flashDismissCmd()
		}
		return m, nil

	case "Y":
		_, absDir := m.cursorModuleDir()
		if absDir != "" {
			m.yankedText = absDir
			m.flash = fmt.Sprintf("Copied: %s", absDir)
			return m, flashDismissCmd()
		}
		return m, nil

	case "c":
		if len(m.selected) == 0 {
			m.flash = "No modules selected"
			return m, flashDismissCmd()
		}
		// Clear all selections — ask for confirmation via confirm overlay.
		m.showConfirm = true
		m.confirmCmd = runner.Command{Name: "clear-selection"}
		return m, nil

	case "L":
		return m.runStateListOnCursor()

	case "C":
		// Clear .terragrunt-cache for selected modules (or cursor module).
		return m.confirmClearCache()

	case "U":
		// Force unlock — prompt for lock ID.
		if m.runner.IsRunning() {
			m.flash = "Command already running — press Esc to cancel first"
			return m, flashDismissCmd()
		}
		relDir, _ := m.cursorModuleDir()
		if relDir == "" {
			m.flash = "Cursor not on a module"
			return m, flashDismissCmd()
		}
		m.unlockModule = relDir
		m.lockIDInput.SetValue("")
		m.lockIDInput.Focus()
		m.showLockIDPrompt = true
		return m, textinput.Blink

	case "X":
		m.flash = "Drift scan: not yet implemented"
		return m, flashDismissCmd()
	case "b":
		m.flash = "Blast radius: not yet implemented"
		return m, flashDismissCmd()

	case "F":
		return m.runOnSelected(runner.CmdHclfmt)

	default:
		return m, nil
	}

	if m.cursor != prevCursor {
		m.loadFileForCursor()
		m.loadDepsForCursor()
	}
	return m, nil
}

func (m Model) collectDescendantModules(treeIdx int) []int {
	var result []int
	var walk func(idx int)
	walk = func(idx int) {
		n := m.tree[idx]
		if n.ModuleIdx >= 0 {
			result = append(result, n.ModuleIdx)
		}
		for _, ci := range n.Children {
			walk(ci)
		}
	}
	walk(treeIdx)
	return result
}

// ---------------------------------------------------------------------------
// [3] State Browser pane keys
// ---------------------------------------------------------------------------

func (m Model) handleStateKey(key string) (tea.Model, tea.Cmd) {
	if len(m.stateResources) == 0 {
		return m, nil
	}

	switch key {
	case "up", "k":
		if m.stateCursor > 0 {
			m.stateCursor--
		}
	case "down", "j":
		if m.stateCursor < len(m.stateResources)-1 {
			m.stateCursor++
		}
	case "home", "g":
		m.stateCursor = 0
	case "end", "G":
		m.stateCursor = len(m.stateResources) - 1
	case "ctrl+d":
		half := m.stateViewHeight() / 2
		m.stateCursor += half
		if m.stateCursor >= len(m.stateResources) {
			m.stateCursor = len(m.stateResources) - 1
		}
	case "ctrl+u":
		half := m.stateViewHeight() / 2
		m.stateCursor -= half
		if m.stateCursor < 0 {
			m.stateCursor = 0
		}

	case "s":
		// State show — output to command log
		resource := m.stateResources[m.stateCursor]
		return m.runStateCmd("state show", []string{"state", "show", resource}, resource)

	case "y":
		// Yank resource name
		resource := m.stateResources[m.stateCursor]
		m.yankedText = resource
		m.flash = fmt.Sprintf("Yanked: %s", resource)
		return m, flashDismissCmd()

	case "t":
		// Taint resource (optimistically mark as tainted)
		resource := m.stateResources[m.stateCursor]
		m.stateTainted[resource] = true
		return m.runStateCmd("taint", []string{"taint", resource}, resource)

	case "u":
		// Untaint resource (optimistically unmark)
		resource := m.stateResources[m.stateCursor]
		delete(m.stateTainted, resource)
		return m.runStateCmd("untaint", []string{"untaint", resource}, resource)

	case "R":
		// Force replace (plan with -replace flag)
		resource := m.stateResources[m.stateCursor]
		return m.runStateCmd("plan -replace", []string{"plan", fmt.Sprintf("-replace=%s", resource)}, resource)

	case "D":
		// Remove from state — needs confirmation
		resource := m.stateResources[m.stateCursor]
		m.showConfirm = true
		m.confirmCmd = runner.Command{
			Name: "state rm",
			Args: []string{"state", "rm", resource},
		}
		return m, nil

	case "C":
		// Clear state browser — needs confirmation
		m.showConfirm = true
		m.confirmCmd = runner.Command{
			Name: "state clear",
		}
		return m, nil

	case "e":
		// Toggle expanded state browser view
		m.stateExpanded = !m.stateExpanded
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// [5] File View pane keys
// ---------------------------------------------------------------------------

func (m Model) handleFileViewKey(key string) (tea.Model, tea.Cmd) {
	lineCount := len(m.fileLines)
	if lineCount == 0 {
		return m, nil
	}

	viewH := m.fileViewHeight()

	switch key {
	case "up", "k":
		if m.fileCursor > 0 {
			m.fileCursor--
		}
	case "down", "j":
		if m.fileCursor < lineCount-1 {
			m.fileCursor++
		}
	case "home", "g":
		m.fileCursor = 0
	case "end", "G":
		m.fileCursor = lineCount - 1
	case "pgup":
		m.fileCursor -= viewH
		if m.fileCursor < 0 {
			m.fileCursor = 0
		}
	case "pgdown":
		m.fileCursor += viewH
		if m.fileCursor >= lineCount {
			m.fileCursor = lineCount - 1
		}
	case "ctrl+d":
		half := viewH / 2
		m.fileCursor += half
		if m.fileCursor >= lineCount {
			m.fileCursor = lineCount - 1
		}
	case "ctrl+u":
		half := viewH / 2
		m.fileCursor -= half
		if m.fileCursor < 0 {
			m.fileCursor = 0
		}
	case "v":
		m.yankStart = m.fileCursor
		m.yankEnd = m.fileCursor
	case "y":
		if m.yankStart >= 0 {
			start := m.yankStart
			end := m.yankEnd
			if start > end {
				start, end = end, start
			}
			if end >= lineCount {
				end = lineCount - 1
			}
			var lines []string
			for i := start; i <= end; i++ {
				lines = append(lines, m.fileLines[i])
			}
			m.yankedText = strings.Join(lines, "\n")
			m.flash = fmt.Sprintf("Yanked %d line(s)", end-start+1)
			m.yankStart = -1
			m.yankEnd = -1

			// Update scroll
			m.fileScroll = m.fileCursor - viewH/2
			if m.fileScroll < 0 {
				m.fileScroll = 0
			}
			ms := maxScroll(lineCount, viewH)
			if m.fileScroll > ms {
				m.fileScroll = ms
			}
			return m, flashDismissCmd()
		}
	}

	// Update yank selection end position
	if m.yankStart >= 0 {
		m.yankEnd = m.fileCursor
	}

	// Calculate scroll to keep cursor centered
	m.fileScroll = m.fileCursor - viewH/2
	if m.fileScroll < 0 {
		m.fileScroll = 0
	}
	ms := maxScroll(lineCount, viewH)
	if m.fileScroll > ms {
		m.fileScroll = ms
	}

	return m, nil
}

// ---------------------------------------------------------------------------
// Command Log pane keys
// ---------------------------------------------------------------------------

func (m Model) handleCmdLogKey(key string) (tea.Model, tea.Cmd) {
	ms := maxScroll(len(m.cmdLogLines), m.cmdLogViewHeight())
	switch key {
	case "up", "k":
		if m.cmdLogScroll > 0 {
			m.cmdLogScroll--
		}
	case "down", "j":
		if m.cmdLogScroll < ms {
			m.cmdLogScroll++
		}
	case "home", "g":
		m.cmdLogScroll = 0
	case "end", "G":
		m.cmdLogScroll = ms
	case "pgup":
		m.cmdLogScroll -= m.cmdLogViewHeight()
		if m.cmdLogScroll < 0 {
			m.cmdLogScroll = 0
		}
	case "pgdown":
		m.cmdLogScroll += m.cmdLogViewHeight()
		if m.cmdLogScroll > ms {
			m.cmdLogScroll = ms
		}
	case "ctrl+d":
		half := m.cmdLogViewHeight() / 2
		m.cmdLogScroll += half
		if m.cmdLogScroll > ms {
			m.cmdLogScroll = ms
		}
	case "ctrl+u":
		half := m.cmdLogViewHeight() / 2
		m.cmdLogScroll -= half
		if m.cmdLogScroll < 0 {
			m.cmdLogScroll = 0
		}
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// [2] Dependencies pane keys (navigate and highlight blocks)
// ---------------------------------------------------------------------------

func (m Model) handleDepsKey(key string) (tea.Model, tea.Cmd) {
	if len(m.currentDeps) == 0 {
		return m, nil
	}

	prevCursor := m.depsCursor

	switch key {
	case "up", "k":
		if m.depsCursor > 0 {
			m.depsCursor--
		}
	case "down", "j":
		if m.depsCursor < len(m.currentDeps)-1 {
			m.depsCursor++
		}
	case "home", "g":
		m.depsCursor = 0
	case "end", "G":
		m.depsCursor = len(m.currentDeps) - 1
	case "ctrl+d":
		half := m.depsViewHeight() / 2
		m.depsCursor += half
		if m.depsCursor >= len(m.currentDeps) {
			m.depsCursor = len(m.currentDeps) - 1
		}
	case "ctrl+u":
		half := m.depsViewHeight() / 2
		m.depsCursor -= half
		if m.depsCursor < 0 {
			m.depsCursor = 0
		}
	case "enter":
		// Jump to the block in file view and focus it
		dep := m.currentDeps[m.depsCursor]
		m.fileCursor = dep.StartLine
		m.focus = paneFileView
		// Update scroll to center the cursor
		viewH := m.fileViewHeight()
		m.fileScroll = m.fileCursor - viewH/2
		if m.fileScroll < 0 {
			m.fileScroll = 0
		}
		return m, nil
	}

	// When cursor changes, scroll file view to show the selected block
	if m.depsCursor != prevCursor && m.depsCursor < len(m.currentDeps) {
		dep := m.currentDeps[m.depsCursor]
		// Center the block in the file view
		viewH := m.fileViewHeight()
		centerScroll := dep.StartLine - viewH/2
		if centerScroll < 0 {
			centerScroll = 0
		}
		maxS := maxScroll(len(m.fileLines), viewH)
		if centerScroll > maxS {
			centerScroll = maxS
		}
		m.fileScroll = centerScroll
	}

	return m, nil
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

func (m Model) handleSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.searchMode = false
		m.searchInput.Blur()
		return m, nil
	case "esc":
		m.searchMode = false
		m.searchInput.Blur()
		m.filterText = ""
		m.searchInput.SetValue("")
		m.rebuildVisible()
		m.cursor = 0
		m.loadFileForCursor()
		m.loadDepsForCursor()
		return m, nil
	}

	var cmd tea.Cmd
	m.searchInput, cmd = m.searchInput.Update(msg)
	m.filterText = m.searchInput.Value()
	m.rebuildVisible()
	if m.cursor >= len(m.visible) {
		m.cursor = max(0, len(m.visible)-1)
	}
	m.loadFileForCursor()
	m.loadDepsForCursor()
	return m, cmd
}

// ---------------------------------------------------------------------------
// Help scroll
// ---------------------------------------------------------------------------

func (m Model) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Popup height: 80% of screen, capped
	maxViewH := m.helpViewHeight()

	// Max scroll position — clamp so we can't scroll past content end.
	maxS := m.helpLineCount - maxViewH
	if maxS < 0 {
		maxS = 0
	}

	switch msg.String() {
	case "j", "down":
		m.helpScroll++
	case "k", "up":
		if m.helpScroll > 0 {
			m.helpScroll--
		}
	case "pgdown":
		m.helpScroll += maxViewH / 2
	case "pgup":
		m.helpScroll -= maxViewH / 2
		if m.helpScroll < 0 {
			m.helpScroll = 0
		}
	case "ctrl+d":
		m.helpScroll += maxViewH / 2
	case "ctrl+u":
		m.helpScroll -= maxViewH / 2
		if m.helpScroll < 0 {
			m.helpScroll = 0
		}
	case "g", "home":
		m.helpScroll = 0
	case "G", "end":
		m.helpScroll = maxS
	default:
		// Any other key closes help
		m.showHelp = false
		m.helpScroll = 0
		return m, nil
	}

	// Clamp.
	if m.helpScroll > maxS {
		m.helpScroll = maxS
	}
	if m.helpScroll < 0 {
		m.helpScroll = 0
	}
	return m, nil
}

// helpViewHeight returns the number of visible content lines in the help popup.
func (m Model) helpViewHeight() int {
	h := m.height*80/100 - 6 // subtract border, title, footer
	if h > 30 {
		h = 30
	}
	if h < 5 {
		h = 5
	}
	return h
}

// ---------------------------------------------------------------------------
// TF_LOG level picker
// ---------------------------------------------------------------------------

// tfLogLevelLabel returns a display label for a TF_LOG level.
// Empty string means OFF (no TF_LOG set).
func tfLogLevelLabel(level string) string {
	if level == "" {
		return "OFF"
	}
	return level
}

func (m Model) handleLogLevelKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		if m.logLevelCursor < len(tfLogLevels)-1 {
			m.logLevelCursor++
		}
	case "k", "up":
		if m.logLevelCursor > 0 {
			m.logLevelCursor--
		}
	case "enter", " ":
		m.tfLogLevel = tfLogLevels[m.logLevelCursor]
		m.showLogLevelPicker = false
		// Update runner's extra env.
		m.syncRunnerLogLevel()
		label := tfLogLevelLabel(m.tfLogLevel)
		m.flash = fmt.Sprintf("TF_LOG set to %s", label)
		return m, flashDismissCmd()
	default:
		// Any other key closes the picker without changing.
		m.showLogLevelPicker = false
	}
	return m, nil
}

// syncRunnerLogLevel updates the runner's extra env to include the current TF_LOG level.
func (m *Model) syncRunnerLogLevel() {
	if m.tfLogLevel == "" {
		m.runner.SetExtraEnv(nil)
	} else {
		m.runner.SetExtraEnv([]string{"TF_LOG=" + m.tfLogLevel})
	}
}

// ---------------------------------------------------------------------------
// Force Unlock — Lock ID text prompt
// ---------------------------------------------------------------------------

func (m Model) handleLockIDKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		lockID := strings.TrimSpace(m.lockIDInput.Value())
		if lockID == "" {
			m.flash = "Lock ID cannot be empty"
			m.showLockIDPrompt = false
			m.lockIDInput.Blur()
			return m, flashDismissCmd()
		}
		m.showLockIDPrompt = false
		m.lockIDInput.Blur()

		// Find the absolute dir for the module.
		var absDir string
		for _, mod := range m.modules {
			if mod.Dir == m.unlockModule {
				absDir = mod.AbsDir
				break
			}
		}
		if absDir == "" {
			m.flash = "Module not found"
			return m, flashDismissCmd()
		}

		cmd := runner.CmdForceUnlock(lockID)
		m.lastCmdName = cmd.Name
		m.runner.Run(cmd, []string{m.unlockModule}, map[string]string{m.unlockModule: absDir})
		m.flash = fmt.Sprintf("Unlocking %s with ID %s...", m.unlockModule, lockID)
		return m, flashDismissCmd()

	case "esc":
		m.showLockIDPrompt = false
		m.lockIDInput.Blur()
		m.flash = "Cancelled"
		return m, flashDismissCmd()
	}

	var cmd tea.Cmd
	m.lockIDInput, cmd = m.lockIDInput.Update(msg)
	return m, cmd
}

// ---------------------------------------------------------------------------
// Clear .terragrunt-cache
// ---------------------------------------------------------------------------

// confirmClearCache prompts the user to confirm clearing .terragrunt-cache
// for selected modules (or cursor module if none selected).
func (m Model) confirmClearCache() (tea.Model, tea.Cmd) {
	dirs, _ := m.selectedModuleDirs()
	if len(dirs) == 0 {
		// Fall back to cursor module.
		relDir, _ := m.cursorModuleDir()
		if relDir == "" {
			m.flash = "No module under cursor"
			return m, flashDismissCmd()
		}
	}
	m.showConfirm = true
	m.confirmCmd = runner.Command{Name: "clear-cache"}
	return m, nil
}

// clearTerragruntCache removes .terragrunt-cache directories from the given
// module directories and logs the results to the command log.
func (m *Model) clearTerragruntCache() tea.Cmd {
	dirs, absMap := m.selectedModuleDirs()
	if len(dirs) == 0 {
		// Fall back to cursor module.
		relDir, absDir := m.cursorModuleDir()
		if relDir == "" {
			return nil
		}
		dirs = []string{relDir}
		absMap = map[string]string{relDir: absDir}
	}

	m.cmdLogLines = append(m.cmdLogLines, cmdLogLine{
		Text: fmt.Sprintf("━━━ Clearing .terragrunt-cache for %d module(s) ━━━", len(dirs)),
	})

	cleared := 0
	for _, relDir := range dirs {
		absDir := absMap[relDir]
		cachePath := filepath.Join(absDir, ".terragrunt-cache")
		info, err := os.Stat(cachePath)
		if err != nil || !info.IsDir() {
			m.cmdLogLines = append(m.cmdLogLines, cmdLogLine{
				Text: fmt.Sprintf("  [%s] no .terragrunt-cache found", relDir),
			})
			continue
		}
		if err := os.RemoveAll(cachePath); err != nil {
			m.cmdLogLines = append(m.cmdLogLines, cmdLogLine{
				Text:    fmt.Sprintf("✗ [%s] failed to remove cache: %v", relDir, err),
				IsError: true,
			})
		} else {
			m.cmdLogLines = append(m.cmdLogLines, cmdLogLine{
				Text: fmt.Sprintf("✓ [%s] cache cleared", relDir),
			})
			cleared++
		}
	}

	m.cmdLogLines = append(m.cmdLogLines, cmdLogLine{
		Text: fmt.Sprintf("━━━ Done — cleared %d/%d ━━━", cleared, len(dirs)),
	})
	m.cmdLogScroll = maxScroll(len(m.cmdLogLines), m.cmdLogViewHeight())
	m.flash = fmt.Sprintf("Cleared cache for %d module(s)", cleared)
	return flashDismissCmd()
}

// ---------------------------------------------------------------------------
// Confirm
// ---------------------------------------------------------------------------

func (m Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.showConfirm = false
		// Check if this is a state clear command
		if m.confirmCmd.Name == "state clear" {
			m.stateResources = nil
			m.stateCursor = 0
			m.stateModule = ""
			m.stateAbsDir = ""
			m.stateTainted = make(map[string]bool)
			m.stateExpanded = false
			m.flash = "State browser cleared"
			return m, flashDismissCmd()
		}
		// Check if this is a state rm command (from State Browser)
		if m.confirmCmd.Name == "state rm" {
			resource := m.confirmCmd.Args[len(m.confirmCmd.Args)-1]
			// Optimistically remove the resource from the state list.
			for i, r := range m.stateResources {
				if r == resource {
					m.stateResources = append(m.stateResources[:i], m.stateResources[i+1:]...)
					break
				}
			}
			delete(m.stateTainted, resource)
			if m.stateCursor >= len(m.stateResources) {
				m.stateCursor = max(0, len(m.stateResources)-1)
			}
			return m.runStateCmd(m.confirmCmd.Name, m.confirmCmd.Args, resource)
		}
		// Check if this is a clear-selection command
		if m.confirmCmd.Name == "clear-selection" {
			count := len(m.selected)
			m.selected = make(map[int]bool)
			m.flash = fmt.Sprintf("Cleared %d selected modules", count)
			return m, flashDismissCmd()
		}
		// Check if this is a clear-cache command
		if m.confirmCmd.Name == "clear-cache" {
			return m, m.clearTerragruntCache()
		}
		return m.runOnSelected(m.confirmCmd)
	default:
		m.showConfirm = false
		m.flash = "Cancelled"
		return m, flashDismissCmd()
	}
}

// ---------------------------------------------------------------------------
// Command execution
// ---------------------------------------------------------------------------

func (m Model) selectedModuleDirs() ([]string, map[string]string) {
	dirs := make([]string, 0, len(m.selected))
	absMap := make(map[string]string)
	for idx := range m.selected {
		mod := m.modules[idx]
		dirs = append(dirs, mod.Dir)
		absMap[mod.Dir] = mod.AbsDir
	}
	sort.Strings(dirs)
	return dirs, absMap
}

func (m Model) runOnSelected(cmd runner.Command) (tea.Model, tea.Cmd) {
	if m.runner.IsRunning() {
		m.flash = "Command already running — press Esc to cancel first"
		return m, flashDismissCmd()
	}
	dirs, absMap := m.selectedModuleDirs()
	if len(dirs) == 0 {
		m.flash = "No modules selected — press Space to select"
		return m, flashDismissCmd()
	}
	m.flash = ""
	m.lastCmdName = cmd.Name

	// Use run-all when multiple modules are selected for more efficient execution
	if len(dirs) > 1 {
		m.runner.RunAll(cmd, dirs, absMap, m.rootDir)
	} else {
		m.runner.Run(cmd, dirs, absMap)
	}
	return m, nil
}

// runAllOnSelected always uses run-all with strict include flags (Ctrl+p / Ctrl+a).
func (m Model) runAllOnSelected(cmd runner.Command) (tea.Model, tea.Cmd) {
	if m.runner.IsRunning() {
		m.flash = "Command already running — press Esc to cancel first"
		return m, flashDismissCmd()
	}
	dirs, absMap := m.selectedModuleDirs()
	if len(dirs) == 0 {
		m.flash = "No modules selected — press Space to select"
		return m, flashDismissCmd()
	}
	m.flash = ""
	m.lastCmdName = cmd.Name
	m.runner.RunAll(cmd, dirs, absMap, m.rootDir)
	return m, nil
}

// openInEditor opens the module's terragrunt.hcl (or root.hcl when openRoot is
// true) in $EDITOR. The TUI is suspended while the editor runs.
func (m Model) openInEditor(openRoot bool) (tea.Model, tea.Cmd) {
	_, absDir := m.cursorModuleDir()
	if absDir == "" {
		m.flash = "Cursor not on a module"
		return m, flashDismissCmd()
	}

	filePath := filepath.Join(absDir, "terragrunt.hcl")
	if openRoot {
		// Walk up to find the root config referenced by the module's include.
		filePath = identity.FindRootHCL(absDir)
		if filePath == "" {
			m.flash = "Could not locate root HCL file"
			return m, flashDismissCmd()
		}
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}

	c := exec.Command(editor, filePath)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return m, tea.ExecProcess(c, func(err error) tea.Msg {
		return editorFinishedMsg{err: err}
	})
}

// runStateListOnCursor runs `terragrunt state list` on the module under the cursor
// asynchronously and returns results via stateListMsg to populate the State Browser.
func (m Model) runStateListOnCursor() (tea.Model, tea.Cmd) {
	if m.runner.IsRunning() {
		m.flash = "Command already running — press Esc to cancel first"
		return m, flashDismissCmd()
	}
	relDir, absDir := m.cursorModuleDir()
	if relDir == "" {
		m.flash = "Cursor not on a module"
		return m, flashDismissCmd()
	}
	m.flash = fmt.Sprintf("Loading state for %s...", relDir)
	// Log the command to the command log.
	m.cmdLogLines = append(m.cmdLogLines,
		cmdLogLine{Text: fmt.Sprintf("─── terragrunt state list [%s] ───", relDir)},
		cmdLogLine{Text: "Running..."},
	)
	m.cmdLogScroll = maxScroll(len(m.cmdLogLines), m.cmdLogViewHeight())
	tfLogLevel := m.tfLogLevel // capture for closure
	cmd := func() tea.Msg {
		c := exec.Command("terragrunt", "state", "list")
		c.Dir = absDir
		env := append(os.Environ(), "TF_IN_AUTOMATION=1")
		if tfLogLevel != "" {
			env = append(env, "TF_LOG="+tfLogLevel)
		}
		c.Env = env
		out, err := c.Output()
		if err != nil {
			return stateListMsg{module: relDir, absDir: absDir, err: err}
		}
		var resources []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				resources = append(resources, line)
			}
		}
		return stateListMsg{module: relDir, absDir: absDir, resources: resources}
	}
	return m, cmd
}

// runStateCmd runs a terragrunt state command on the currently loaded state module.
// Output goes to the command log. Used for state show, taint, untaint, plan -replace, state rm.
func (m Model) runStateCmd(name string, args []string, _ string) (tea.Model, tea.Cmd) {
	if m.runner.IsRunning() {
		m.flash = "Command already running — press Esc to cancel first"
		return m, flashDismissCmd()
	}
	if m.stateAbsDir == "" || m.stateModule == "" {
		m.flash = "No state loaded — press Shift+L on a module first"
		return m, flashDismissCmd()
	}

	cmd := runner.Command{Name: name, Args: args}
	m.lastCmdName = name
	m.runner.Run(cmd, []string{m.stateModule}, map[string]string{m.stateModule: m.stateAbsDir})
	return m, nil
}

func (m Model) confirmAndRun(cmd runner.Command) (tea.Model, tea.Cmd) {
	if m.runner.IsRunning() {
		m.flash = "Command already running — press Esc to cancel first"
		return m, flashDismissCmd()
	}
	dirs, _ := m.selectedModuleDirs()
	if len(dirs) == 0 {
		m.flash = "No modules selected"
		return m, flashDismissCmd()
	}
	m.showConfirm = true
	m.confirmCmd = cmd
	return m, nil
}

// prodDestructiveGuard returns true with a warning message when the current
// module targets a prod environment. This serves as a speed-bump for
// destructive operations — the user must confirm before proceeding.
func (m Model) prodDestructiveGuard() (bool, string) {
	if m.headerCtx.EnvLevel != identity.EnvProd {
		return false, ""
	}
	return true, "Warning: this is a PROD environment (" + m.headerCtx.Env + ")"
}

// ---------------------------------------------------------------------------
// Height helpers
// ---------------------------------------------------------------------------

func (m Model) fileViewHeight() int {
	contentHeight := m.height - 1
	h := contentHeight*70/100 - 3
	if h < 1 {
		h = 1
	}
	return h
}

func (m Model) cmdLogViewHeight() int {
	contentHeight := m.height - 1
	var cmdLogH int
	if m.focus == paneCmdLog || m.focus == paneState {
		cmdLogH = contentHeight
	} else {
		rightTopH := contentHeight * 70 / 100
		cmdLogH = contentHeight - rightTopH
	}
	h := cmdLogH - 3
	if h < 1 {
		h = 1
	}
	return h
}

// modulesViewHeight returns the visible line count in the modules pane.
func (m Model) modulesViewHeight() int {
	contentHeight := m.height - 2
	infoH := 3
	remainH := contentHeight - infoH
	h := remainH*60/100 - 2 // subtract border
	if h < 1 {
		h = 1
	}
	return h
}

// depsViewHeight returns the visible line count in the dependencies pane.
func (m Model) depsViewHeight() int {
	contentHeight := m.height - 2
	infoH := 3
	remainH := contentHeight - infoH
	h := remainH*20/100 - 2
	if h < 1 {
		h = 1
	}
	return h
}

// stateViewHeight returns the visible line count in the state browser pane.
func (m Model) stateViewHeight() int {
	contentHeight := m.height - 2
	infoH := 3
	remainH := contentHeight - infoH
	modulesH := remainH * 60 / 100
	depsH := remainH * 20 / 100
	if depsH < 4 {
		depsH = 4
	}
	h := remainH - modulesH - depsH - 2
	if h < 1 {
		h = 1
	}
	return h
}

func maxScroll(totalLines, viewHeight int) int {
	s := totalLines - viewHeight
	if s < 0 {
		return 0
	}
	return s
}

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7C3AED")).
			Padding(0, 1)

	normalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#CDD6F4"))

	cursorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#CDD6F4")).
			Background(lipgloss.Color("#45475A"))

	selectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A6E3A1"))

	cursorSelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#A6E3A1")).
				Background(lipgloss.Color("#45475A"))

	dirStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#89B4FA")).
			Bold(true)

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6C7086"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F38BA8"))

	successStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A6E3A1"))

	runningStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F9E2AF"))

	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#585B70"))

	activeBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#7C3AED"))

	flashStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F9E2AF")).
			Bold(true)

	confirmStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F38BA8")).
			Bold(true)

	helpKeyStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#89B4FA")).
			Bold(true)

	helpDescStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#CDD6F4"))

	paneLabelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F9E2AF")).
			Bold(true)

	paneLabelDimStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#6C7086")).
				Bold(true)

	yankHighlight = lipgloss.NewStyle().
			Background(lipgloss.Color("#45475A"))

	depNameStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F5C2E7")).
			Bold(true)

	depTypeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#CBA6F7"))

	versionLabelStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#89B4FA")).
				Bold(true)

	versionValueStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#A6E3A1"))

	cmdLogOutputStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#BAC2DE"))

	cmdLogHeaderStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#89B4FA")).
				Bold(true)

	helpSectionStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#CBA6F7"))

	// Header bar styles — env badge colors
	headerProdStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#1E1E2E")).
			Background(lipgloss.Color("#F38BA8")) // red bg for prod

	headerStagingStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#1E1E2E")).
				Background(lipgloss.Color("#F9E2AF")) // amber bg for staging

	headerNeutralStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#CDD6F4")).
				Background(lipgloss.Color("#45475A")) // dim bg for dev/neutral

	headerSepStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#585B70"))

	headerValueStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#CDD6F4"))

	headerLabelStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#6C7086"))
)

// ---------------------------------------------------------------------------
// Header bar — cloud execution context
// ---------------------------------------------------------------------------

func (m Model) headerBar() string {
	ctx := m.headerCtx
	sep := headerSepStyle.Render(" | ")
	var parts []string

	// Environment badge — always shown if we have an env label.
	if ctx.Env != "" {
		label := " env: " + ctx.Env + " "
		switch ctx.EnvLevel {
		case identity.EnvProd:
			parts = append(parts, headerProdStyle.Render(label))
		case identity.EnvStaging:
			parts = append(parts, headerStagingStyle.Render(label))
		default:
			parts = append(parts, headerNeutralStyle.Render(label))
		}
	}

	// Profile
	if ctx.Profile != "" {
		parts = append(parts, headerLabelStyle.Render("profile: ")+
			headerValueStyle.Render(ctx.Profile))
	}

	// Region
	if ctx.Region != "" {
		parts = append(parts, headerValueStyle.Render(ctx.Region))
	}

	if len(parts) == 0 {
		// No context available — show a minimal placeholder.
		return lipgloss.NewStyle().
			Width(m.width).
			Padding(0, 1).
			Foreground(lipgloss.Color("#585B70")).
			Render("lazyterra")
	}

	content := strings.Join(parts, sep)
	return lipgloss.NewStyle().
		Width(m.width).
		Padding(0, 1).
		Render(content)
}

// truncateLine truncates a string to fit within maxWidth, adding "..." if needed.
// It operates on rune count to handle multi-byte characters.
func truncateLine(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxWidth {
		return s
	}
	if maxWidth <= 3 {
		return string(runes[:maxWidth])
	}
	return string(runes[:maxWidth-3]) + "..."
}

// wrapText splits a string into multiple lines, each at most maxWidth runes.
// If the input fits in one line, returns a single-element slice.
func wrapText(s string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{""}
	}
	runes := []rune(s)
	if len(runes) <= maxWidth {
		return []string{s}
	}
	var lines []string
	for len(runes) > maxWidth {
		lines = append(lines, string(runes[:maxWidth]))
		runes = runes[maxWidth:]
	}
	if len(runes) > 0 {
		lines = append(lines, string(runes))
	}
	return lines
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading..."
	}

	// Header bar (1 line) + content + status bar (1 line)
	header := m.headerBar()
	contentHeight := m.height - 2

	// Expanded State Browser: full-width layout (state on top, cmd log on bottom)
	if m.stateExpanded && m.focus == paneState {
		fullW := m.width - 2 // border
		stateH := contentHeight * 65 / 100
		cmdLogH := contentHeight - stateH
		if cmdLogH < 4 {
			cmdLogH = 4
			stateH = contentHeight - cmdLogH
		}

		getBorder := func(p focusPane) lipgloss.Style {
			if m.focus == p {
				return activeBorderStyle
			}
			return borderStyle
		}

		stateContent := m.stateView(fullW, stateH-2)
		statePane := getBorder(paneState).Width(fullW).Height(stateH - 2).MaxHeight(stateH).Render(stateContent)

		cmdLogContent := m.cmdLogView(fullW, cmdLogH-2)
		cmdPane := getBorder(paneCmdLog).Width(fullW).Height(cmdLogH - 2).MaxHeight(cmdLogH).Render(cmdLogContent)

		body := lipgloss.JoinVertical(lipgloss.Left, statePane, cmdPane)
		status := m.statusBar()
		base := lipgloss.JoinVertical(lipgloss.Left, header, body, status)
		if m.showHelp {
			return m.overlayHelp(base)
		}
		if m.showConfirm {
			return m.overlayConfirm(base)
		}
		if m.showLogLevelPicker {
			return m.overlayLogLevel(base)
		}
		if m.showLockIDPrompt {
			return m.overlayLockIDPrompt(base)
		}
		return base
	}

	// Layout:
	//
	// ┌── Left Column ────────┐ ┌── Right Column ────────┐
	// │ [1] Modules    (55%)  │ │ [5] File View    (70%)  │
	// │                       │ │                         │
	// ├───────────────────────┤ ├─────────────────────────┤
	// │ [2] Dependencies(15%) │ │  Command Log     (30%)  │
	// ├───────────────────────┤ │  (@ to focus)           │
	// │ [3] State Brows.(15%) │ └─────────────────────────┘
	// ├───────────────────────┤
	// │ [4] Info       (15%)  │
	// └───────────────────────┘
	//
	// When Command Log is focused, it expands to fill the entire right column.

	// Fixed left column width: 30% of screen (reduced from 40%)
	leftWidth := m.width * 30 / 100
	if leftWidth < 28 {
		leftWidth = 28
	}
	if leftWidth > 50 {
		leftWidth = 50
	}
	rightWidth := m.width - leftWidth - 1

	// Fixed pane heights — info is minimal (title only)
	infoH := 3
	remainH := contentHeight - infoH
	modulesH := remainH * 60 / 100
	depsH := remainH * 20 / 100
	stateH := remainH - modulesH - depsH
	if depsH < 4 {
		depsH = 4
	}
	if stateH < 4 {
		stateH = 4
	}

	// Right column: when Command Log or State Browser is focused, cmd log expands to full height
	var fileViewH, cmdLogH int
	if m.focus == paneCmdLog || m.focus == paneState {
		fileViewH = 0
		cmdLogH = contentHeight
	} else {
		fileViewH = contentHeight * 70 / 100
		cmdLogH = contentHeight - fileViewH
	}

	// Inner content dimensions (subtract 2 for border).
	lw := leftWidth - 2
	rw := rightWidth - 2

	modulesContent := m.modulesView(lw, modulesH-2)
	depsContent := m.depsView(lw, depsH-2)
	stateContent := m.stateView(lw, stateH-2)
	infoContent := m.infoView(lw, infoH-2)

	getBorder := func(p focusPane) lipgloss.Style {
		if m.focus == p {
			return activeBorderStyle
		}
		return borderStyle
	}

	modulesPane := getBorder(paneModules).Width(lw).Height(modulesH - 2).MaxHeight(modulesH).Render(modulesContent)
	depsPane := getBorder(paneDeps).Width(lw).Height(depsH - 2).MaxHeight(depsH).Render(depsContent)
	statePane := getBorder(paneState).Width(lw).Height(stateH - 2).MaxHeight(stateH).Render(stateContent)
	infoPane := getBorder(paneInfo).Width(lw).Height(infoH - 2).MaxHeight(infoH).Render(infoContent)

	leftColumn := lipgloss.JoinVertical(lipgloss.Left, modulesPane, depsPane, statePane, infoPane)

	// Right column: conditionally show File View
	var rightColumn string
	switch m.focus {
	case paneCmdLog, paneState:
		cmdLogContent := m.cmdLogView(rw, cmdLogH-2)
		cmdPane := getBorder(paneCmdLog).Width(rw).Height(cmdLogH - 2).MaxHeight(cmdLogH).Render(cmdLogContent)
		rightColumn = cmdPane
	case paneInfo:
		infoExpandedContent := m.infoExpandedView(rw, contentHeight-2)
		infoExpandedPane := getBorder(paneInfo).Width(rw).Height(contentHeight - 2).MaxHeight(contentHeight).Render(infoExpandedContent)
		rightColumn = infoExpandedPane
	default:
		fileViewContent := m.fileViewPane(rw, fileViewH-2)
		cmdLogContent := m.cmdLogView(rw, cmdLogH-2)
		filePane := getBorder(paneFileView).Width(rw).Height(fileViewH - 2).MaxHeight(fileViewH).Render(fileViewContent)
		cmdPane := getBorder(paneCmdLog).Width(rw).Height(cmdLogH - 2).MaxHeight(cmdLogH).Render(cmdLogContent)
		rightColumn = lipgloss.JoinVertical(lipgloss.Left, filePane, cmdPane)
	}

	body := lipgloss.JoinHorizontal(lipgloss.Top, leftColumn, rightColumn)

	status := m.statusBar()

	base := lipgloss.JoinVertical(lipgloss.Left, header, body, status)
	if m.showHelp {
		return m.overlayHelp(base)
	}
	if m.showConfirm {
		return m.overlayConfirm(base)
	}
	if m.showLogLevelPicker {
		return m.overlayLogLevel(base)
	}
	if m.showLockIDPrompt {
		return m.overlayLockIDPrompt(base)
	}
	return base
}

// ---------------------------------------------------------------------------
// Pane header helpers
// ---------------------------------------------------------------------------

func (m Model) paneHeader(p focusPane, extra string) string {
	num := paneNumber(p)
	var label string
	if num > 0 {
		label = fmt.Sprintf("[%d] %s", num, paneName(p))
	} else if p == paneCmdLog {
		label = "[@] " + paneName(p)
	} else {
		label = paneName(p)
	}
	if extra != "" {
		label += " " + extra
	}
	if m.focus == p {
		return paneLabelStyle.Render(label)
	}
	return paneLabelDimStyle.Render(label)
}

// ---------------------------------------------------------------------------
// [1] Modules pane
// ---------------------------------------------------------------------------

func (m Model) modulesView(width, height int) string {
	var b strings.Builder

	selCount := len(m.selected)

	// Build custom title line 1: [1] Modules (sel/total) selected
	labelStyle := paneLabelDimStyle
	if m.focus == paneModules {
		labelStyle = paneLabelStyle
	}
	title := labelStyle.Render(fmt.Sprintf("[1] Modules (%d/%d) selected", selCount, len(m.modules)))
	b.WriteString(title)
	b.WriteString("\n")
	height--

	// Line 2: View Mode: [tree] [flat]
	activeToggle := lipgloss.NewStyle().Foreground(lipgloss.Color("#A6E3A1")).Bold(true)
	inactiveToggle := lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086"))
	treeLabel := inactiveToggle.Render("[tree]")
	flatLabel := inactiveToggle.Render("[flat]")
	if !m.modulesFlat {
		treeLabel = activeToggle.Render("[tree]")
	} else {
		flatLabel = activeToggle.Render("[flat]")
	}
	viewLine := dimStyle.Render("View Mode:") + " " + treeLabel + " " + flatLabel
	b.WriteString(viewLine)
	b.WriteString("\n")
	height--

	if m.searchMode {
		b.WriteString(m.searchInput.View())
		b.WriteString("\n")
		height--
	} else if m.filterText != "" {
		b.WriteString(dimStyle.Render(fmt.Sprintf("filter: %s", m.filterText)))
		b.WriteString("\n")
		height--
	}

	if len(m.visible) == 0 {
		b.WriteString(dimStyle.Render("  No modules found"))
		return b.String()
	}

	// Calculate scroll position to keep cursor centered (within ~40-60% of view)
	vis := height
	if vis < 1 {
		vis = 1
	}

	// Calculate ideal start position to center the cursor
	start := 0
	if len(m.visible) <= vis {
		// All items fit, no scrolling needed
		start = 0
	} else {
		// Center the cursor
		idealStart := m.cursor - vis/2
		if idealStart < 0 {
			idealStart = 0
		}
		maxStart := len(m.visible) - vis
		if idealStart > maxStart {
			idealStart = maxStart
		}
		start = idealStart
	}

	end := start + vis
	if end > len(m.visible) {
		end = len(m.visible)
	}

	if m.modulesFlat {
		// Flat path view: show full relative path for each module (no dirs)
		for vi := start; vi < end; vi++ {
			treeIdx := m.visible[vi]
			node := m.tree[treeIdx]
			isCursor := (vi == m.cursor) && (m.focus == paneModules)

			// In flat mode we only show modules
			if node.ModuleIdx < 0 {
				continue
			}

			isSelected := m.selected[node.ModuleIdx]

			sel := "○"
			if isSelected {
				sel = "●"
			}

			mod := m.modules[node.ModuleIdx]
			st := m.runner.GetStatus(mod.Dir)
			statusChar := " "
			switch st {
			case runner.StatusRunning:
				statusChar = "~"
			case runner.StatusSuccess:
				statusChar = "+"
			case runner.StatusError:
				statusChar = "x"
			}

			// Show path in flat view, truncated to fit pane width
			pathDisplay := truncateLine(node.FullPath, width-5) // 5 = " O S " prefix
			line := fmt.Sprintf(" %s %s %s", sel, statusChar, pathDisplay)

			if isCursor && isSelected {
				line = cursorSelectedStyle.Render(line)
			} else if isCursor {
				line = cursorStyle.Render(line)
			} else if isSelected {
				line = selectedStyle.Render(line)
			} else {
				statusDisplay := " "
				switch st {
				case runner.StatusRunning:
					statusDisplay = runningStyle.Render("~")
				case runner.StatusSuccess:
					statusDisplay = successStyle.Render("+")
				case runner.StatusError:
					statusDisplay = errorStyle.Render("x")
				}
				line = fmt.Sprintf(" %s %s %s",
					normalStyle.Render(sel),
					statusDisplay,
					normalStyle.Render(pathDisplay))
			}

			lineLen := lipgloss.Width(line)
			if lineLen < width && isCursor {
				pad := strings.Repeat(" ", width-lineLen)
				if m.selected[node.ModuleIdx] {
					line += cursorSelectedStyle.Render(pad)
				} else {
					line += cursorStyle.Render(pad)
				}
			}

			b.WriteString(line)
			if vi < end-1 {
				b.WriteString("\n")
			}
		}
	} else {
		// Tree view (original)
		for vi := start; vi < end; vi++ {
			treeIdx := m.visible[vi]
			node := m.tree[treeIdx]
			indent := strings.Repeat("  ", node.Depth)
			isCursor := (vi == m.cursor) && (m.focus == paneModules)

			var line string
			if node.IsDir {
				arrow := "▸"
				if !node.Collapsed {
					arrow = "▾"
				}
				line = fmt.Sprintf("%s%s %s/", indent, arrow, node.Name)
				if isCursor {
					line = cursorStyle.Render(line)
				} else {
					line = dirStyle.Render(line)
				}
			} else {
				isSelected := m.selected[node.ModuleIdx]

				sel := "○"
				if isSelected {
					sel = "●"
				}

				mod := m.modules[node.ModuleIdx]
				st := m.runner.GetStatus(mod.Dir)
				statusChar := " "
				switch st {
				case runner.StatusRunning:
					statusChar = "~"
				case runner.StatusSuccess:
					statusChar = "+"
				case runner.StatusError:
					statusChar = "x"
				}

				line = fmt.Sprintf("%s%s %s %s", indent, sel, statusChar, node.Name)

				if isCursor && isSelected {
					line = cursorSelectedStyle.Render(line)
				} else if isCursor {
					line = cursorStyle.Render(line)
				} else if isSelected {
					line = selectedStyle.Render(line)
				} else {
					statusDisplay := " "
					switch st {
					case runner.StatusRunning:
						statusDisplay = runningStyle.Render("~")
					case runner.StatusSuccess:
						statusDisplay = successStyle.Render("+")
					case runner.StatusError:
						statusDisplay = errorStyle.Render("x")
					}
					line = fmt.Sprintf("%s%s %s %s", indent,
						normalStyle.Render(sel),
						statusDisplay,
						normalStyle.Render(node.Name))
				}
			}

			lineLen := lipgloss.Width(line)
			if lineLen < width && isCursor {
				pad := strings.Repeat(" ", width-lineLen)
				if node.ModuleIdx >= 0 && m.selected[node.ModuleIdx] {
					line += cursorSelectedStyle.Render(pad)
				} else {
					line += cursorStyle.Render(pad)
				}
			}

			b.WriteString(line)
			if vi < end-1 {
				b.WriteString("\n")
			}
		}
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// [2] Dependencies pane
// ---------------------------------------------------------------------------

func (m Model) depsView(width, height int) string {
	var b strings.Builder

	b.WriteString(m.paneHeader(paneDeps, ""))
	b.WriteString("\n")
	height--

	if len(m.currentDeps) == 0 {
		b.WriteString(dimStyle.Render("  No dependencies detected"))
		return b.String()
	}

	// Calculate scroll position to keep cursor visible
	start := 0
	vis := height
	if vis < 1 {
		vis = 1
	}
	if m.depsCursor >= start+vis {
		start = m.depsCursor - vis + 1
	}
	if m.depsCursor < start {
		start = m.depsCursor
	}
	end := start + vis
	if end > len(m.currentDeps) {
		end = len(m.currentDeps)
	}

	for i := start; i < end; i++ {
		d := m.currentDeps[i]
		isCursor := (i == m.depsCursor) && (m.focus == paneDeps)

		if isCursor {
			// Build plain text (no inner ANSI) so cursorStyle background is uninterrupted
			plainLine := fmt.Sprintf("  [%s] %s", d.Type, d.Label)
			line := cursorStyle.Render(plainLine)
			lineLen := lipgloss.Width(line)
			if lineLen < width {
				pad := strings.Repeat(" ", width-lineLen)
				line += cursorStyle.Render(pad)
			}
			b.WriteString(line)
		} else {
			typeLabel := depTypeStyle.Render(fmt.Sprintf("[%s]", d.Type))
			name := depNameStyle.Render(d.Label)
			line := fmt.Sprintf("  %s %s", typeLabel, name)
			b.WriteString(line)
		}

		if i < end-1 {
			b.WriteString("\n")
		}
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// [3] State Browser pane
// ---------------------------------------------------------------------------

func (m Model) stateView(width, height int) string {
	var b strings.Builder

	b.WriteString(m.paneHeader(paneState, ""))
	b.WriteString("\n")
	height--

	if m.stateModule != "" {
		modText := fmt.Sprintf("  %s", m.stateModule)
		modText = truncateLine(modText, width)
		modLine := dimStyle.Render(modText)
		b.WriteString(modLine)
		b.WriteString("\n")
		height--
	}

	if len(m.stateResources) == 0 {
		b.WriteString(dimStyle.Render("  No state loaded — press Shift+L on a module"))
		return b.String()
	}

	// Calculate scroll position to keep cursor visible
	start := 0
	vis := height
	if vis < 1 {
		vis = 1
	}

	// Center cursor in view
	if len(m.stateResources) <= vis {
		start = 0
	} else {
		idealStart := m.stateCursor - vis/2
		if idealStart < 0 {
			idealStart = 0
		}
		maxStart := len(m.stateResources) - vis
		if idealStart > maxStart {
			idealStart = maxStart
		}
		start = idealStart
	}

	end := start + vis
	if end > len(m.stateResources) {
		end = len(m.stateResources)
	}

	for i := start; i < end; i++ {
		resource := m.stateResources[i]
		isCursor := (i == m.stateCursor) && (m.focus == paneState)

		// Build plain text line then truncate to fit width
		var plainLine string
		if m.stateTainted[resource] {
			plainLine = fmt.Sprintf("t %s", resource)
		} else {
			plainLine = fmt.Sprintf("  %s", resource)
		}
		plainLine = truncateLine(plainLine, width)

		var line string
		if isCursor {
			line = cursorStyle.Render(plainLine)
			lineLen := lipgloss.Width(line)
			if lineLen < width {
				pad := strings.Repeat(" ", width-lineLen)
				line += cursorStyle.Render(pad)
			}
		} else if m.stateTainted[resource] {
			// Taint marker in yellow, rest normal
			taintMark := lipgloss.NewStyle().Foreground(lipgloss.Color("#F9E2AF")).Render("t ")
			line = taintMark + normalStyle.Render(plainLine[2:])
		} else {
			line = normalStyle.Render(plainLine)
		}

		b.WriteString(line)
		if i < end-1 {
			b.WriteString("\n")
		}
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// [4] Info pane
// ---------------------------------------------------------------------------

func (m Model) infoView(_, _ int) string {
	var b strings.Builder

	b.WriteString(m.paneHeader(paneInfo, ""))

	return b.String()
}

// infoExpandedView renders the full-screen Info panel on the right side when [4] Info is focused.
func (m Model) infoExpandedView(_, height int) string {
	var b strings.Builder

	b.WriteString(m.paneHeader(paneInfo, ""))
	b.WriteString("\n")
	height--

	// ASCII logo
	logo := []string{
		"  ██╗      █████╗ ███████╗██╗   ██╗",
		"  ██║     ██╔══██╗╚══███╔╝╚██╗ ██╔╝",
		"  ██║     ███████║  ███╔╝  ╚████╔╝ ",
		"  ██║     ██╔══██║ ███╔╝    ╚██╔╝  ",
		"  ███████╗██║  ██║███████╗   ██║   ",
		"  ╚══════╝╚═╝  ╚═╝╚══════╝   ╚═╝   ",
		"  ████████╗███████╗██████╗ ██████╗  █████╗ ",
		"  ╚══██╔══╝██╔════╝██╔══██╗██╔══██╗██╔══██╗",
		"     ██║   █████╗  ██████╔╝██████╔╝███████║",
		"     ██║   ██╔══╝  ██╔══██╗██╔══██╗██╔══██║",
		"     ██║   ███████╗██║  ██║██║  ██║██║  ██║",
		"     ╚═╝   ╚══════╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝",
	}

	for _, line := range logo {
		if height <= 0 {
			break
		}
		b.WriteString(titleStyle.Render(line))
		b.WriteString("\n")
		height--
	}

	b.WriteString("\n")
	height--

	// Subtitle
	b.WriteString(dimStyle.Render("  A TUI for Terragrunt"))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(fmt.Sprintf("  Version %s", m.appVersion)))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("  Copyright 2026 Nikita Palnov"))
	b.WriteString("\n\n")
	height -= 4

	// Versions
	fmt.Fprintf(&b, "  %s %s\n",
		versionLabelStyle.Render("Terraform:"),
		versionValueStyle.Render(m.versionInfo.Terraform))
	fmt.Fprintf(&b, "  %s %s\n",
		versionLabelStyle.Render("Terragrunt:"),
		versionValueStyle.Render(m.versionInfo.Terragrunt))
	b.WriteString("\n")
	height -= 3

	// Links
	b.WriteString(dimStyle.Render("  https://github.com/NikitaForGit/LazyTerra"))
	b.WriteString("\n\n")
	height -= 2

	// Quick start
	if height > 14 {
		b.WriteString(helpSectionStyle.Render("  Quick Start"))
		b.WriteString("\n")
		tips := []string{
			"  " + helpKeyStyle.Render("1") + " Modules",
			"  " + helpKeyStyle.Render("2") + " Dependencies",
			"  " + helpKeyStyle.Render("3") + " State Browser",
			"  " + helpKeyStyle.Render("4") + " Info",
			"  " + helpKeyStyle.Render("5") + " File View",
			"  " + helpKeyStyle.Render("@") + " Command Log",
			"",
			"  " + helpKeyStyle.Render("Tab") + "       Next Pane",
			"  " + helpKeyStyle.Render("Shift+Tab") + " Previous Pane",
			"",
			"  " + helpKeyStyle.Render("?") + " Show Help (On Focused Pane)",
		}
		for _, tip := range tips {
			if height <= 0 {
				break
			}
			b.WriteString(tip)
			b.WriteString("\n")
			height--
		}
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// [5] File View pane (with HCL syntax highlighting)
// ---------------------------------------------------------------------------

func (m Model) fileViewPane(width, height int) string {
	var b strings.Builder

	b.WriteString(m.paneHeader(paneFileView, ""))
	b.WriteString("\n")
	height--

	// File path on its own line
	if m.filePath != "" {
		b.WriteString(dimStyle.Render(m.filePath))
		b.WriteString("\n")
		height--
	}

	if len(m.fileLines) == 0 {
		if len(m.visible) > 0 && m.cursor < len(m.visible) {
			treeIdx := m.visible[m.cursor]
			node := m.tree[treeIdx]
			if node.IsDir {
				b.WriteString(dimStyle.Render("  Select a module to view its terragrunt.hcl"))
			} else {
				b.WriteString(dimStyle.Render("  No file content"))
			}
		} else {
			b.WriteString(dimStyle.Render("  No module selected"))
		}
		return b.String()
	}

	end := m.fileScroll + height
	if end > len(m.fileLines) {
		end = len(m.fileLines)
	}
	start := m.fileScroll
	if start > end {
		start = end
	}

	// Yank selection range
	yankLo, yankHi := -1, -1
	if m.yankStart >= 0 {
		yankLo = m.yankStart
		yankHi = m.yankEnd
		if yankLo > yankHi {
			yankLo, yankHi = yankHi, yankLo
		}
	}

	// Dependency block highlight range (when deps pane is focused)
	depBlockLo, depBlockHi := -1, -1
	if m.focus == paneDeps && len(m.currentDeps) > 0 && m.depsCursor < len(m.currentDeps) {
		dep := m.currentDeps[m.depsCursor]
		depBlockLo = dep.StartLine
		depBlockHi = dep.EndLine
	}

	for i := start; i < end; i++ {
		content := m.fileLines[i]
		isCursor := (i == m.fileCursor) && (m.focus == paneFileView)

		maxContentW := width - 1
		if maxContentW > 0 {
			runes := []rune(content)
			if len(runes) > maxContentW {
				content = string(runes[:maxContentW-3]) + "..."
			}
		}

		// Determine styling based on selection/highlight state
		if i >= yankLo && i <= yankHi && yankLo >= 0 {
			// Yank selection takes priority — highlight entire line
			styled := yankHighlight.Render(content)
			// Pad to fill width
			lineLen := lipgloss.Width(styled)
			if lineLen < width {
				styled += yankHighlight.Render(strings.Repeat(" ", width-lineLen))
			}
			b.WriteString(styled)
		} else if i >= depBlockLo && i <= depBlockHi && depBlockLo >= 0 {
			// Highlight dependency block with a subtle background (use plain text)
			styled := yankHighlight.Render(content)
			lineLen := lipgloss.Width(styled)
			if lineLen < width {
				styled += yankHighlight.Render(strings.Repeat(" ", width-lineLen))
			}
			b.WriteString(styled)
		} else if isCursor {
			// Strip ANSI from highlighted content so cursorStyle background is uninterrupted
			plain := content
			styled := cursorStyle.Render(plain)
			lineLen := lipgloss.Width(styled)
			if lineLen < width {
				styled += cursorStyle.Render(strings.Repeat(" ", width-lineLen))
			}
			b.WriteString(styled)
		} else {
			// Apply HCL syntax highlighting
			highlighted := highlightHCL(content)
			b.WriteString(highlighted)
		}

		if i < end-1 {
			b.WriteString("\n")
		}
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// Command Log pane (full output, toggled with @)
// ---------------------------------------------------------------------------

func (m Model) cmdLogView(width, height int) string {
	var b strings.Builder

	extra := ""
	if m.runner.IsRunning() {
		extra = runningStyle.Render("(running...)")
	}
	b.WriteString(m.paneHeader(paneCmdLog, extra))
	b.WriteString("\n")
	height--

	if len(m.cmdLogLines) == 0 {
		b.WriteString(dimStyle.Render("  No commands executed yet. Focus with ") + paneLabelStyle.Render("@"))
		return b.String()
	}

	// Build visual lines by wrapping long command log entries.
	// We keep cmdLogScroll as a logical line index and map to visual offset.
	type visualLine struct {
		text    string
		isError bool
		isFirst bool // first line of a logical entry (for styling prefix detection)
		rawText string
	}
	var vLines []visualLine
	// Map logical line index -> visual line offset
	logicalToVisual := make([]int, len(m.cmdLogLines)+1)
	for li, entry := range m.cmdLogLines {
		logicalToVisual[li] = len(vLines)
		wrapped := wrapText(entry.Text, width)
		for j, wl := range wrapped {
			vLines = append(vLines, visualLine{
				text:    wl,
				isError: entry.IsError,
				isFirst: j == 0,
				rawText: entry.Text,
			})
		}
	}
	logicalToVisual[len(m.cmdLogLines)] = len(vLines)

	// When not focused, always pin to bottom.
	logScroll := m.cmdLogScroll
	if m.focus != paneCmdLog {
		logScroll = len(m.cmdLogLines) // will clamp below
	}
	// Clamp logical scroll
	if logScroll > len(m.cmdLogLines) {
		logScroll = len(m.cmdLogLines)
	}
	if logScroll < 0 {
		logScroll = 0
	}

	// Convert to visual offset
	visStart := 0
	if logScroll < len(logicalToVisual) {
		visStart = logicalToVisual[logScroll]
	}

	// Pin to bottom: when unfocused always, or when focused and scrolled to max.
	ms := maxScroll(len(m.cmdLogLines), m.cmdLogViewHeight())
	if m.focus != paneCmdLog || m.cmdLogScroll >= ms {
		visStart = len(vLines) - height
		if visStart < 0 {
			visStart = 0
		}
	}

	visEnd := visStart + height
	if visEnd > len(vLines) {
		visEnd = len(vLines)
	}

	for i := visStart; i < visEnd; i++ {
		vl := vLines[i]
		text := vl.text

		if vl.isError {
			text = errorStyle.Render(text)
		} else if vl.isFirst && strings.HasPrefix(strings.TrimSpace(vl.rawText), "━") {
			text = cmdLogHeaderStyle.Render(text)
		} else if vl.isFirst && strings.HasPrefix(strings.TrimSpace(vl.rawText), "▶") {
			text = runningStyle.Render(text)
		} else if vl.isFirst && strings.HasPrefix(strings.TrimSpace(vl.rawText), "✓") {
			text = successStyle.Render(text)
		} else if vl.isFirst && strings.HasPrefix(strings.TrimSpace(vl.rawText), "✗") {
			text = errorStyle.Render(text)
		} else {
			// Apply some basic highlighting to terraform/terragrunt output.
			text = highlightCmdOutput(text)
		}

		b.WriteString(text)
		if i < visEnd-1 {
			b.WriteString("\n")
		}
	}

	return b.String()
}

// highlightCmdOutput applies basic syntax-like coloring to command output lines.
func highlightCmdOutput(line string) string {
	trimmed := strings.TrimSpace(line)

	// plan-style coloring.
	if strings.HasPrefix(trimmed, "+") || (strings.HasPrefix(trimmed, "# ") && strings.Contains(trimmed, "will be created")) {
		return successStyle.Render(line)
	}
	if strings.HasPrefix(trimmed, "-") || strings.Contains(trimmed, "will be destroyed") {
		return errorStyle.Render(line)
	}
	if strings.HasPrefix(trimmed, "~") || strings.Contains(trimmed, "will be updated") {
		return runningStyle.Render(line)
	}
	if strings.Contains(strings.ToLower(trimmed), "error") || strings.Contains(strings.ToLower(trimmed), "failed") {
		return errorStyle.Render(line)
	}
	if strings.Contains(trimmed, "Plan:") || strings.Contains(trimmed, "Apply complete") || strings.Contains(trimmed, "No changes") {
		return cmdLogHeaderStyle.Render(line)
	}

	return cmdLogOutputStyle.Render(line)
}

// ---------------------------------------------------------------------------
// Status bar
// ---------------------------------------------------------------------------

func (m Model) statusBar() string {
	var left string
	if m.flash != "" {
		left = flashStyle.Render(m.flash)
	} else {
		// Context-aware keybindings based on focused pane
		var keys []string

		switch m.focus {
		case paneModules:
			keys = []string{
				helpKeyStyle.Render("Space") + " select",
				helpKeyStyle.Render("p") + " plan",
				helpKeyStyle.Render("a") + " apply",
				helpKeyStyle.Render("i") + " init",
				helpKeyStyle.Render("/") + " filter",
			}
		case paneDeps:
			keys = []string{
				helpKeyStyle.Render("j/k") + " navigate",
				helpKeyStyle.Render("Ctrl+u/d") + " up/down",
				helpKeyStyle.Render("Enter") + " focus block",
			}
		case paneState:
			keys = []string{
				helpKeyStyle.Render("e") + " expand view",
				helpKeyStyle.Render("s") + " show",
				helpKeyStyle.Render("y") + " yank",
				helpKeyStyle.Render("t/u") + " taint/untaint",
			}
		case paneInfo:
			keys = []string{
				helpKeyStyle.Render("Tab") + " next pane",
			}
		case paneFileView:
			keys = []string{
				helpKeyStyle.Render("v") + " selection mode",
				helpKeyStyle.Render("y") + " yank",
				helpKeyStyle.Render("Ctrl+u/d") + " up/down",
				helpKeyStyle.Render("g") + " top",
				helpKeyStyle.Render("G") + " bottom",
			}
		case paneCmdLog:
			keys = []string{
				helpKeyStyle.Render("j/k") + " scroll",
				helpKeyStyle.Render("Ctrl+u/d") + " up/down",
				helpKeyStyle.Render("g") + " top",
				helpKeyStyle.Render("G") + " bottom",
			}
		}

		// Always add help
		keys = append(keys,
			helpKeyStyle.Render("?")+" help",
		)

		left = strings.Join(keys, dimStyle.Render(" | "))
	}

	return lipgloss.NewStyle().
		Width(m.width).
		Padding(0, 1).
		Render(left)
}

// ---------------------------------------------------------------------------
// Help overlay — contextual per pane.
// Renders ON TOP of the base TUI using painter's algorithm.
// ---------------------------------------------------------------------------

// overlayHelp composites the help popup on top of the base TUI string.
func (m Model) overlayHelp(base string) string {
	return m.overlayPopup(base, m.helpView())
}

// overlayConfirm renders the confirmation dialog on top of the base TUI.
func (m Model) overlayConfirm(base string) string {
	return m.overlayPopup(base, m.confirmView())
}

// overlayLogLevel renders the TF_LOG level picker on top of the base TUI.
func (m Model) overlayLogLevel(base string) string {
	return m.overlayPopup(base, m.logLevelPickerView())
}

// overlayLockIDPrompt renders the force-unlock lock ID input on top of the base TUI.
func (m Model) overlayLockIDPrompt(base string) string {
	return m.overlayPopup(base, m.lockIDPromptView())
}

// logLevelPickerView renders the TF_LOG level picker box.
func (m Model) logLevelPickerView() string {
	title := helpSectionStyle.Render("TF_LOG Level")

	var b strings.Builder
	for i, lvl := range tfLogLevels {
		label := tfLogLevelLabel(lvl)

		// Show a marker for the currently active level.
		activeMarker := " "
		if lvl == m.tfLogLevel {
			activeMarker = "●"
		}

		if i == m.logLevelCursor {
			// Cursor line: highlighted.
			line := fmt.Sprintf(" %s  %s", activeMarker, label)
			b.WriteString(cursorStyle.Render(line))
			// Pad to fill popup width.
			lineW := lipgloss.Width(cursorStyle.Render(line))
			if lineW < 26 {
				b.WriteString(cursorStyle.Render(strings.Repeat(" ", 26-lineW)))
			}
		} else {
			marker := dimStyle.Render(activeMarker)
			if activeMarker == "●" {
				marker = successStyle.Render(activeMarker)
			}
			fmt.Fprintf(&b, " %s  %s", marker, normalStyle.Render(label))
		}
		if i < len(tfLogLevels)-1 {
			b.WriteString("\n")
		}
	}

	innerContent := title + "\n\n" + b.String()

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#89B4FA")).
		Width(32).
		Padding(1, 2).
		Render(innerContent)

	return box
}

// lockIDPromptView renders the force-unlock lock ID input popup.
func (m Model) lockIDPromptView() string {
	title := helpSectionStyle.Render("Force Unlock")

	var b strings.Builder
	b.WriteString(dimStyle.Render("Module: "))
	b.WriteString(normalStyle.Render(m.unlockModule))
	b.WriteString("\n\n")
	b.WriteString(dimStyle.Render("Enter lock ID (from error output):"))
	b.WriteString("\n")
	b.WriteString(m.lockIDInput.View())
	b.WriteString("\n\n")
	b.WriteString(dimStyle.Render("Press Enter to unlock, Esc to cancel"))

	innerContent := title + "\n\n" + b.String()

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#F9E2AF")).
		Width(56).
		Padding(1, 2).
		Render(innerContent)

	return box
}

// overlayPopup renders a popup box centered on top of the base TUI string.
func (m Model) overlayPopup(base, popup string) string {
	// Split both into lines.
	baseLines := strings.Split(base, "\n")
	popupLines := strings.Split(popup, "\n")

	// Calculate popup dimensions (visual width of longest popup line).
	popupH := len(popupLines)
	popupW := 0
	for _, line := range popupLines {
		w := lipgloss.Width(line)
		if w > popupW {
			popupW = w
		}
	}

	// Center the popup on the base.
	startRow := (m.height - popupH) / 2
	startCol := (m.width - popupW) / 2
	if startRow < 0 {
		startRow = 0
	}
	if startCol < 0 {
		startCol = 0
	}

	// Pad base lines to at least m.height
	for len(baseLines) < m.height {
		baseLines = append(baseLines, "")
	}

	// Paint popup lines over base lines.
	for i, popupLine := range popupLines {
		row := startRow + i
		if row >= len(baseLines) {
			break
		}
		baseLine := baseLines[row]
		baseLines[row] = overlayLine(baseLine, popupLine, startCol)
	}

	return strings.Join(baseLines[:m.height], "\n")
}

// overlayLine paints foreground text over background text at the given column.
// It pads the background if needed, then replaces characters at the specified position.
func overlayLine(bg, fg string, col int) string {
	// Ensure bg is wide enough.
	bgW := lipgloss.Width(bg)
	if bgW < col {
		bg += strings.Repeat(" ", col-bgW)
	}

	// Build result: bg[:col] + fg + bg[col+fgW:]
	// Since bg may contain ANSI codes, we need to work with visual positions.
	// Simple approach: pad bg to full width as plain cells, then overlay.
	// For simplicity and correctness with ANSI, use lipgloss.PlaceHorizontal.
	fgW := lipgloss.Width(fg)
	afterCol := col + fgW

	// Extract the left portion of bg (first col visual characters).
	leftBg := truncateVisual(bg, col)
	// Extract the right portion of bg (everything after col+fgW).
	rightBg := skipVisual(bg, afterCol)

	return leftBg + fg + rightBg
}

// truncateVisual returns the first n visual columns of a styled string.
func truncateVisual(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// Use lipgloss to measure. Strip ANSI and count runes for position.
	result := strings.Builder{}
	col := 0
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			result.WriteRune(r)
			continue
		}
		if inEscape {
			result.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		if col >= n {
			break
		}
		result.WriteRune(r)
		col++
	}
	// Reset ANSI at the cut point so overlay starts clean.
	result.WriteString("\x1b[0m")
	return result.String()
}

// skipVisual skips the first n visual columns and returns the rest.
func skipVisual(s string, n int) string {
	if n <= 0 {
		return s
	}
	col := 0
	inEscape := false
	for i, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		if col >= n {
			return s[i:]
		}
		col++
	}
	return ""
}

// helpView renders the help popup box (without background placement).
func (m *Model) helpView() string {
	pName := paneName(m.focus)
	num := paneNumber(m.focus)
	var titleLabel string
	if num > 0 {
		titleLabel = fmt.Sprintf("[%d] %s — Help", num, pName)
	} else {
		titleLabel = fmt.Sprintf("%s (@) — Help", pName)
	}

	var bindings [][]string

	// Pane-specific bindings.
	switch m.focus {
	case paneModules:
		bindings = [][]string{
			{"Navigation", ""},
			{"j", "Cursor down"},
			{"k", "Cursor up"},
			{"g", "Go to top"},
			{"G", "Go to bottom"},
			{"Ctrl+D", "Half-page down"},
			{"Ctrl+U", "Half-page up"},
			{"h", "Collapse / go to parent"},
			{"l", "Expand directory"},
			{"Enter", "Toggle dir / open file view"},
			{"Space", "Toggle selection"},
			{"1", "Toggle tree / flat view"},
			{"/", "Search modules"},
			{"", ""},
			{"Operations", ""},
			{"p", "Plan cursor module"},
			{"P", "Plan-all (2+ selected)"},
			{"a", "Apply single (confirm)"},
			{"A", "Apply-all (2+ selected, confirm)"},
			{"d", "Destroy single (confirm)"},
			{"D", "Destroy-all (confirm)"},
			{"i", "Init"},
			{"I", "Init --reconfigure"},
			{"v", "Validate"},
			{"o", "Show outputs"},
			{"r", "Refresh plan"},
			{"", ""},
			{"Tools", ""},
			{"L", "Open state browser"},
			{"e", "Edit module hcl (uses $EDITOR or vi)"},
			{"E", "Edit root hcl (uses $EDITOR or vi)"},
			{"y", "Copy relative path"},
			{"Y", "Copy full path"},
			{"c", "Clear selection (confirm)"},
			{"F", "Run hclfmt on selected"},
			{"C", "Clear .terragrunt-cache"},
			{"U", "Force unlock (enter lock ID)"},
		}
	case paneDeps:
		bindings = [][]string{
			{"Dependencies", ""},
			{"j", "Cursor down"},
			{"k", "Cursor up"},
			{"g", "Go to first block"},
			{"G", "Go to last block"},
			{"Ctrl+D", "Half-page down"},
			{"Ctrl+U", "Half-page up"},
			{"Enter", "Jump to block in file view"},
		}
	case paneState:
		bindings = [][]string{
			{"State Browser", ""},
			{"j", "Cursor down"},
			{"k", "Cursor up"},
			{"g", "Go to first resource"},
			{"G", "Go to last resource"},
			{"Ctrl+D", "Half-page down"},
			{"Ctrl+U", "Half-page up"},
			{"s", "Show resource details"},
			{"y", "Yank resource address"},
			{"t", "Taint resource"},
			{"u", "Untaint resource"},
			{"R", "Replace resource"},
			{"D", "Remove from state (confirm)"},
			{"C", "Clear state browser"},
			{"e", "Toggle expanded view"},
			{"r", "Refresh state list"},
		}
	case paneInfo:
		bindings = [][]string{
			{"Info", ""},
			{"", "(app info, versions, links)"},
		}
	case paneFileView:
		bindings = [][]string{
			{"File View", ""},
			{"j", "Scroll down"},
			{"k", "Scroll up"},
			{"g", "Go to top"},
			{"G", "Go to bottom"},
			{"Ctrl+D", "Half-page down"},
			{"Ctrl+U", "Half-page up"},
			{"PgUp", "Page up"},
			{"PgDn", "Page down"},
			{"v", "Visual line select"},
			{"y", "Yank selected lines"},
			{"Esc", "Cancel selection"},
		}
	case paneCmdLog:
		bindings = [][]string{
			{"Command Log", ""},
			{"j", "Scroll down"},
			{"k", "Scroll up"},
			{"g", "Go to top"},
			{"G", "Go to bottom"},
			{"Ctrl+D", "Half-page down"},
			{"Ctrl+U", "Half-page up"},
			{"PgUp", "Page up"},
			{"PgDn", "Page down"},
		}
	}

	// Always add global bindings.
	bindings = append(bindings, []string{"", ""})
	bindings = append(bindings, [][]string{
		{"Global", ""},
		{"Tab", "Next pane"},
		{"Shift+Tab", "Previous pane"},
		{"1-5", "Jump to pane"},
		{"@", "Focus command log"},
		{"T", "TF_LOG level picker"},
		{"/", "Search modules"},
		{"?", "Toggle help"},
		{"Esc", "Cancel running command"},
		{"q", "Quit"},
		{"Q", "Kill all + quit"},
	}...)

	var b strings.Builder
	for _, pair := range bindings {
		if len(pair) < 2 {
			continue
		}
		if pair[1] == "" {
			if pair[0] != "" {
				b.WriteString("\n" + helpSectionStyle.Render(pair[0]) + "\n")
			}
			continue
		}
		if pair[0] == "" {
			b.WriteString(" " + dimStyle.Render(pair[1]) + "\n")
			continue
		}
		fmt.Fprintf(&b, " %s %s\n",
			helpKeyStyle.Width(10).Align(lipgloss.Right).Render(pair[0]),
			helpDescStyle.Render(pair[1]),
		)
	}

	// Split into lines for scrolling.
	allLines := strings.Split(b.String(), "\n")
	m.helpLineCount = len(allLines)

	maxViewH := m.helpViewHeight()

	// Clamp scroll.
	maxS := len(allLines) - maxViewH
	if maxS < 0 {
		maxS = 0
	}
	if m.helpScroll > maxS {
		m.helpScroll = maxS
	}

	start := m.helpScroll
	end := start + maxViewH
	if end > len(allLines) {
		end = len(allLines)
	}
	visibleLines := allLines[start:end]

	// Join visible lines (no inline scrollbar — keeps layout clean).
	visibleContent := strings.Join(visibleLines, "\n")

	// Title inside the box, above scrollable content.
	titleLine := helpSectionStyle.Render(titleLabel)
	if maxS > 0 {
		pct := 0
		if maxS > 0 {
			pct = m.helpScroll * 100 / maxS
		}
		titleLine += "  " + dimStyle.Render(fmt.Sprintf("%d%%", pct))
	}

	innerContent := titleLine + "\n\n" + visibleContent

	popupW := 56
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7C3AED")).
		Width(popupW).
		Padding(1, 2).
		Render(innerContent)

	return box
}

// ---------------------------------------------------------------------------
// Confirm overlay
// ---------------------------------------------------------------------------

func (m Model) confirmView() string {
	var msg string

	// Show prod environment warning banner if applicable.
	if isProd, reason := m.prodDestructiveGuard(); isProd {
		msg += headerProdStyle.Render(" ⚠ "+reason+" ") + "\n\n"
	}

	switch m.confirmCmd.Name {
	case "state clear":
		msg += "Are you sure you want to clear the state browser?"
	case "state rm":
		resource := ""
		if len(m.confirmCmd.Args) > 2 {
			resource = m.confirmCmd.Args[len(m.confirmCmd.Args)-1]
		}
		msg += "Are you sure you want to remove from state?\n\n"
		msg += fmt.Sprintf("  Module:   %s\n", m.stateModule)
		msg += fmt.Sprintf("  Resource: %s\n", resource)
	case "clear-selection":
		msg += fmt.Sprintf("Clear all %d selected modules?", len(m.selected))
	case "clear-cache":
		dirs, _ := m.selectedModuleDirs()
		if len(dirs) == 0 {
			relDir, _ := m.cursorModuleDir()
			if relDir != "" {
				dirs = []string{relDir}
			}
		}
		msg += fmt.Sprintf("Clear .terragrunt-cache for %d module(s)?\n\n", len(dirs))
		for _, d := range dirs {
			msg += fmt.Sprintf("  - %s\n", d)
		}
	default:
		dirs, _ := m.selectedModuleDirs()
		msg += fmt.Sprintf("Are you sure you want to run terragrunt %s on %d module(s)?\n\n", m.confirmCmd.Name, len(dirs))
		for _, d := range dirs {
			msg += fmt.Sprintf("  - %s\n", d)
		}
	}
	msg += "\nPress 'y' to confirm, any other key to cancel."

	content := confirmStyle.Render(msg)
	box := borderStyle.
		Width(60).
		Padding(1, 2).
		BorderForeground(lipgloss.Color("#F38BA8")).
		Render(content)

	return box
}

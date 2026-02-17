package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/lazyterra/lazyterra/internal/deps"
	"github.com/lazyterra/lazyterra/internal/discovery"
	"github.com/lazyterra/lazyterra/internal/git"
	"github.com/lazyterra/lazyterra/internal/runner"
	"github.com/lazyterra/lazyterra/internal/version"
)

// ---------------------------------------------------------------------------
// Pane identifiers
//
//	[1] Modules   [2] Dependencies   [3] Branches   [4] Info   [5] File View
//	Command Log is unnumbered — toggled with @
// ---------------------------------------------------------------------------

type focusPane int

const (
	paneModules  focusPane = iota // [1]
	paneDeps                      // [2]
	paneBranches                  // [3]
	paneInfo                      // [4]
	paneFileView                  // [5]
	paneCmdLog                    // not numbered, toggled with @

	// Number of numbered panes (used for 1-5 key switching).
	numberedPanes = 5
)

func paneName(p focusPane) string {
	switch p {
	case paneModules:
		return "Modules"
	case paneDeps:
		return "Dependencies"
	case paneBranches:
		return "Branches"
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
	case paneBranches:
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
	Text     string
	IsError  bool
	IsHeader bool // separator / header lines
}

// ---------------------------------------------------------------------------
// Tree node (lazygit-style collapsible directory tree)
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
	hclCommentRe = regexp.MustCompile(`(#.*|//.*|/\*.*\*/)`)
	hclBoolRe    = regexp.MustCompile(`\b(true|false|null)\b`)
	hclNumberRe  = regexp.MustCompile(`\b\d+\b`)

	synKeyword = lipgloss.NewStyle().Foreground(lipgloss.Color("#CBA6F7"))              // purple
	synBlock   = lipgloss.NewStyle().Foreground(lipgloss.Color("#89B4FA"))              // blue
	synFunc    = lipgloss.NewStyle().Foreground(lipgloss.Color("#F9E2AF"))              // yellow
	synString  = lipgloss.NewStyle().Foreground(lipgloss.Color("#A6E3A1"))              // green
	synComment = lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7086")).Italic(true) // dim
	synBool    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FAB387"))              // orange
	synNumber  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FAB387"))              // orange
	synPunct   = lipgloss.NewStyle().Foreground(lipgloss.Color("#CDD6F4"))
)

// highlightHCL applies syntax coloring to a single line of HCL.
// We process from highest to lowest priority to avoid overlapping matches.
func highlightHCL(line string) string {
	if strings.TrimSpace(line) == "" {
		return line
	}

	// If the whole line is a comment, style it all at once.
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
		return synComment.Render(line)
	}

	// We'll do a simple token-replacement approach.
	// To avoid ANSI nesting issues, we replace tokens with placeholders,
	// then swap them back at the end.
	type replacement struct {
		start int
		end   int
		text  string
	}

	var reps []replacement

	// Strings first (highest priority — don't recolor inside strings).
	for _, loc := range hclStringRe.FindAllStringIndex(line, -1) {
		reps = append(reps, replacement{loc[0], loc[1], synString.Render(line[loc[0]:loc[1]])})
	}

	// If line has strings, just color the strings and leave the rest with
	// keyword/block coloring applied to non-string parts.
	// For simplicity, we'll build the output segment by segment.
	if len(reps) > 0 {
		var out strings.Builder
		prev := 0
		for _, r := range reps {
			if r.start > prev {
				out.WriteString(highlightHCLSegment(line[prev:r.start]))
			}
			out.WriteString(r.text)
			prev = r.end
		}
		if prev < len(line) {
			out.WriteString(highlightHCLSegment(line[prev:]))
		}
		return out.String()
	}

	return highlightHCLSegment(line)
}

// highlightHCLSegment colors a segment that contains no string literals.
func highlightHCLSegment(s string) string {
	// Apply keywords.
	s = hclKeywordRe.ReplaceAllStringFunc(s, func(m string) string {
		return synKeyword.Render(m)
	})
	s = hclBlockRe.ReplaceAllStringFunc(s, func(m string) string {
		return synBlock.Render(m)
	})
	s = hclBoolRe.ReplaceAllStringFunc(s, func(m string) string {
		return synBool.Render(m)
	})
	s = hclNumberRe.ReplaceAllStringFunc(s, func(m string) string {
		return synNumber.Render(m)
	})
	// Functions — color just the function name, keep the paren.
	s = hclFuncRe.ReplaceAllStringFunc(s, func(m string) string {
		// m ends with '(' — color everything before it.
		return synFunc.Render(m[:len(m)-1]) + "("
	})
	return s
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type Model struct {
	// Data
	modules []discovery.Module
	tree    []treeNode
	visible []int

	selected map[int]bool
	cursor   int

	// Branches
	branches      []git.Branch
	branchCursor  int
	currentBranch string
	isGitRepo     bool

	// Runner
	runner *runner.Runner

	// Command log (full output lines + headers)
	cmdLogLines  []cmdLogLine
	cmdLogScroll int
	lastCmdName  string

	// File view
	fileLines  []string
	fileScroll int
	filePath   string
	yankStart  int
	yankEnd    int
	yankedText string

	// Dependencies
	currentDeps []deps.Dependency

	// Versions
	versionInfo version.Info

	// UI state
	focus       focusPane
	width       int
	height      int
	showHelp    bool
	showConfirm bool
	confirmCmd  runner.Command
	searchMode  bool
	searchInput textinput.Model
	filterText  string

	flash string
}

func NewModel(modules []discovery.Module) Model {
	ti := textinput.New()
	ti.Placeholder = "search..."
	ti.CharLimit = 128

	tree := buildTree(modules)
	vis := visibleNodes(tree)

	m := Model{
		modules:   modules,
		tree:      tree,
		visible:   vis,
		selected:  make(map[int]bool),
		runner:    runner.New(),
		focus:     paneModules,
		yankStart: -1,
		yankEnd:   -1,
	}
	m.searchInput = ti

	if git.IsGitRepo() {
		m.isGitRepo = true
		m.refreshBranches()
	}

	m.versionInfo = version.Detect()
	m.loadFileForCursor()
	m.loadDepsForCursor()

	return m
}

func (m *Model) refreshBranches() {
	branches, err := git.ListBranches()
	if err != nil {
		m.branches = nil
		return
	}
	m.branches = branches
	for _, b := range branches {
		if b.IsCurrent {
			m.currentBranch = b.Name
			break
		}
	}
}

func (m *Model) rebuildVisible() {
	if m.filterText == "" {
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
}

func (m *Model) loadDepsForCursor() {
	m.currentDeps = nil

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

type outputMsg runner.OutputLine
type resultMsg runner.Result

type branchSwitchedMsg struct {
	branch string
	err    error
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

	case branchSwitchedMsg:
		if msg.err != nil {
			m.flash = fmt.Sprintf("Checkout failed: %v", msg.err)
		} else {
			m.currentBranch = msg.branch
			m.flash = fmt.Sprintf("Switched to branch: %s", msg.branch)
			m.refreshBranches()
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
	if m.searchMode {
		return m.handleSearchKey(msg)
	}
	if m.showHelp {
		m.showHelp = false
		return m, nil
	}

	key := msg.String()

	// Global keys.
	switch key {
	case "q", "ctrl+c":
		m.runner.Cancel()
		return m, tea.Quit

	case "?":
		m.showHelp = true
		return m, nil

	case "tab":
		// Cycle through numbered panes only (skip cmdLog).
		switch m.focus {
		case paneModules:
			m.focus = paneDeps
		case paneDeps:
			m.focus = paneBranches
		case paneBranches:
			m.focus = paneInfo
		case paneInfo:
			m.focus = paneFileView
		case paneFileView:
			m.focus = paneModules
		case paneCmdLog:
			m.focus = paneModules
		}
		return m, nil

	case "1":
		m.focus = paneModules
		return m, nil
	case "2":
		m.focus = paneDeps
		return m, nil
	case "3":
		m.focus = paneBranches
		return m, nil
	case "4":
		m.focus = paneInfo
		return m, nil
	case "5":
		m.focus = paneFileView
		return m, nil
	case "@":
		m.focus = paneCmdLog
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
		}
		return m, nil
	}

	switch m.focus {
	case paneModules:
		return m.handleModuleKey(key)
	case paneBranches:
		return m.handleBranchKey(key)
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

	case " ":
		treeIdx := m.visible[m.cursor]
		node := &m.tree[treeIdx]
		if node.ModuleIdx >= 0 {
			if m.selected[node.ModuleIdx] {
				delete(m.selected, node.ModuleIdx)
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

	case "l":
		if m.cursor < visCount {
			treeIdx := m.visible[m.cursor]
			node := &m.tree[treeIdx]
			if node.IsDir {
				node.Collapsed = false
				m.rebuildVisible()
			}
		}
		return m, nil

	case "h":
		if m.cursor < visCount {
			treeIdx := m.visible[m.cursor]
			node := &m.tree[treeIdx]
			if node.IsDir && !node.Collapsed {
				node.Collapsed = true
				m.rebuildVisible()
			} else if node.Parent >= 0 {
				m.tree[node.Parent].Collapsed = true
				m.rebuildVisible()
				for vi, vi2 := range m.visible {
					if vi2 == node.Parent {
						m.cursor = vi
						break
					}
				}
			}
		}
		if m.cursor != prevCursor {
			m.loadFileForCursor()
			m.loadDepsForCursor()
		}
		return m, nil

	case "a":
		if len(m.selected) == len(m.modules) {
			m.selected = make(map[int]bool)
		} else {
			for i := range m.modules {
				m.selected[i] = true
			}
		}
		return m, nil

	case "P":
		return m.runOnSelected(runner.CmdPlan)
	case "A":
		return m.runOnSelected(runner.CmdApply)
	case "V":
		return m.runOnSelected(runner.CmdValidate)
	case "i":
		return m.runOnSelected(runner.CmdInit)
	case "I":
		return m.runOnSelected(runner.CmdInitReconfigure)
	case "D":
		return m.confirmAndRun(runner.CmdDestroy)
	case "O":
		return m.runOnSelected(runner.CmdOutput)
	case "L":
		return m.runStateListOnCursor()

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
// [3] Branch pane keys
// ---------------------------------------------------------------------------

func (m Model) handleBranchKey(key string) (tea.Model, tea.Cmd) {
	if len(m.branches) == 0 {
		return m, nil
	}

	switch key {
	case "up", "k":
		if m.branchCursor > 0 {
			m.branchCursor--
		}
	case "down", "j":
		if m.branchCursor < len(m.branches)-1 {
			m.branchCursor++
		}
	case "home", "g":
		m.branchCursor = 0
	case "end", "G":
		m.branchCursor = len(m.branches) - 1
	case "enter", " ":
		branch := m.branches[m.branchCursor]
		if branch.IsCurrent {
			m.flash = "Already on this branch"
			return m, nil
		}
		m.flash = fmt.Sprintf("Switching to %s...", branch.Name)
		branchName := branch.Name
		return m, func() tea.Msg {
			err := git.Checkout(branchName)
			return branchSwitchedMsg{branch: branchName, err: err}
		}
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
	ms := maxScroll(lineCount, viewH)

	switch key {
	case "up", "k":
		if m.fileScroll > 0 {
			m.fileScroll--
		}
	case "down", "j":
		if m.fileScroll < ms {
			m.fileScroll++
		}
	case "home", "g":
		m.fileScroll = 0
	case "end", "G":
		m.fileScroll = ms
	case "pgup":
		m.fileScroll -= viewH
		if m.fileScroll < 0 {
			m.fileScroll = 0
		}
	case "pgdown":
		m.fileScroll += viewH
		if m.fileScroll > ms {
			m.fileScroll = ms
		}
	case "v":
		m.yankStart = m.fileScroll
		m.yankEnd = m.fileScroll
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
		}
	}

	if m.yankStart >= 0 {
		m.yankEnd = m.fileScroll
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
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// [2] Dependencies pane keys (scroll only)
// ---------------------------------------------------------------------------

func (m Model) handleDepsKey(key string) (tea.Model, tea.Cmd) {
	// deps is read-only; no keys needed
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
// Confirm
// ---------------------------------------------------------------------------

func (m Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.showConfirm = false
		return m.runOnSelected(m.confirmCmd)
	default:
		m.showConfirm = false
		m.flash = "Cancelled"
		return m, nil
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
		return m, nil
	}
	dirs, absMap := m.selectedModuleDirs()
	if len(dirs) == 0 {
		m.flash = "No modules selected — press Space to select"
		return m, nil
	}
	m.flash = ""
	m.lastCmdName = cmd.Name
	m.runner.Run(cmd, dirs, absMap)
	return m, nil
}

// runStateListOnCursor runs `terragrunt state list` on the module under the cursor.
func (m Model) runStateListOnCursor() (tea.Model, tea.Cmd) {
	if m.runner.IsRunning() {
		m.flash = "Command already running — press Esc to cancel first"
		return m, nil
	}
	relDir, absDir := m.cursorModuleDir()
	if relDir == "" {
		m.flash = "Cursor not on a module"
		return m, nil
	}
	m.flash = ""
	m.lastCmdName = runner.CmdStateList.Name
	m.runner.Run(runner.CmdStateList, []string{relDir}, map[string]string{relDir: absDir})
	return m, nil
}

func (m Model) confirmAndRun(cmd runner.Command) (tea.Model, tea.Cmd) {
	if m.runner.IsRunning() {
		m.flash = "Command already running — press Esc to cancel first"
		return m, nil
	}
	dirs, _ := m.selectedModuleDirs()
	if len(dirs) == 0 {
		m.flash = "No modules selected"
		return m, nil
	}
	m.showConfirm = true
	m.confirmCmd = cmd
	return m, nil
}

// ---------------------------------------------------------------------------
// Height helpers
// ---------------------------------------------------------------------------

func (m Model) fileViewHeight() int {
	contentHeight := m.height - 3
	h := contentHeight*65/100 - 3
	if h < 1 {
		h = 1
	}
	return h
}

func (m Model) cmdLogViewHeight() int {
	contentHeight := m.height - 3
	rightTopH := contentHeight * 65 / 100
	rightBottomH := contentHeight - rightTopH
	h := rightBottomH - 3
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
			Background(lipgloss.Color("#1E1E2E")).
			Padding(0, 1)

	normalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#CDD6F4"))

	cursorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#1E1E2E")).
			Background(lipgloss.Color("#89B4FA"))

	selectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A6E3A1"))

	cursorSelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#A6E3A1")).
				Background(lipgloss.Color("#89B4FA"))

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

	branchCurrentStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#A6E3A1")).
				Bold(true)

	lineNumStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#585B70"))

	yankHighlight = lipgloss.NewStyle().
			Background(lipgloss.Color("#45475A"))

	depNameStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F5C2E7")).
			Bold(true)

	depTypeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#CBA6F7"))

	depPathStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6C7086"))

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
)

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading..."
	}
	if m.showHelp {
		return m.helpView()
	}
	if m.showConfirm {
		return m.confirmView()
	}

	// Layout:
	//
	// ┌── Left Column ────────┐ ┌── Right Column ────────┐
	// │ [1] Modules    (45%)  │ │ [5] File View    (65%)  │
	// │                       │ │                         │
	// ├───────────────────────┤ ├─────────────────────────┤
	// │ [2] Dependencies(20%) │ │  Command Log     (35%)  │
	// ├───────────────────────┤ │  (@ to focus)           │
	// │ [3] Branches   (18%)  │ └─────────────────────────┘
	// ├───────────────────────┤
	// │ [4] Info        (7%)  │
	// └───────────────────────┘

	leftWidth := m.width * 2 / 5
	if leftWidth < 30 {
		leftWidth = 30
	}
	rightWidth := m.width - leftWidth - 1
	contentHeight := m.height - 3

	modulesH := contentHeight * 45 / 100
	depsH := contentHeight * 22 / 100
	branchesH := contentHeight * 18 / 100
	infoH := contentHeight - modulesH - depsH - branchesH
	if infoH < 4 {
		infoH = 4
	}

	fileViewH := contentHeight * 65 / 100
	cmdLogH := contentHeight - fileViewH

	// Inner content dimensions (subtract 2 for border).
	lw := leftWidth - 2
	rw := rightWidth - 2

	modulesContent := m.modulesView(lw, modulesH-2)
	depsContent := m.depsView(lw, depsH-2)
	branchesContent := m.branchesView(lw, branchesH-2)
	infoContent := m.infoView(lw, infoH-2)

	fileViewContent := m.fileViewPane(rw, fileViewH-2)
	cmdLogContent := m.cmdLogView(rw, cmdLogH-2)

	getBorder := func(p focusPane) lipgloss.Style {
		if m.focus == p {
			return activeBorderStyle
		}
		return borderStyle
	}

	modulesPane := getBorder(paneModules).Width(lw).Height(modulesH - 2).Render(modulesContent)
	depsPane := getBorder(paneDeps).Width(lw).Height(depsH - 2).Render(depsContent)
	branchesPane := getBorder(paneBranches).Width(lw).Height(branchesH - 2).Render(branchesContent)
	infoPane := getBorder(paneInfo).Width(lw).Height(infoH - 2).Render(infoContent)

	filePane := getBorder(paneFileView).Width(rw).Height(fileViewH - 2).Render(fileViewContent)
	cmdPane := getBorder(paneCmdLog).Width(rw).Height(cmdLogH - 2).Render(cmdLogContent)

	leftColumn := lipgloss.JoinVertical(lipgloss.Left, modulesPane, depsPane, branchesPane, infoPane)
	rightColumn := lipgloss.JoinVertical(lipgloss.Left, filePane, cmdPane)
	body := lipgloss.JoinHorizontal(lipgloss.Top, leftColumn, rightColumn)

	title := titleStyle.Width(m.width).Render(" LazyTerra — Terragrunt TUI")
	status := m.statusBar()

	return lipgloss.JoinVertical(lipgloss.Left, title, body, status)
}

// ---------------------------------------------------------------------------
// Pane header helpers
// ---------------------------------------------------------------------------

func (m Model) paneHeader(p focusPane, extra string) string {
	num := paneNumber(p)
	var label string
	if num > 0 {
		label = fmt.Sprintf("[%d] %s", num, paneName(p))
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
	b.WriteString(m.paneHeader(paneModules, dimStyle.Render(fmt.Sprintf("(%d/%d)", selCount, len(m.modules)))))
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

	start := 0
	vis := height
	if vis < 1 {
		vis = 1
	}
	if m.cursor >= start+vis {
		start = m.cursor - vis + 1
	}
	if m.cursor < start {
		start = m.cursor
	}
	end := start + vis
	if end > len(m.visible) {
		end = len(m.visible)
	}

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

	for i, d := range m.currentDeps {
		if i >= height {
			remaining := len(m.currentDeps) - i
			b.WriteString(dimStyle.Render(fmt.Sprintf("  ... and %d more", remaining)))
			break
		}

		typeLabel := depTypeStyle.Render(fmt.Sprintf("[%s]", d.Type))
		name := depNameStyle.Render(d.Label)
		path := depPathStyle.Render(d.ConfigPath)
		line := fmt.Sprintf("  %s %s -> %s", typeLabel, name, path)

		b.WriteString(line)
		if i < len(m.currentDeps)-1 && i < height-1 {
			b.WriteString("\n")
		}
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// [3] Branches pane
// ---------------------------------------------------------------------------

func (m Model) branchesView(width, height int) string {
	var b strings.Builder

	extra := ""
	if m.currentBranch != "" {
		extra = dimStyle.Render(fmt.Sprintf("(on: %s)", m.currentBranch))
	}
	b.WriteString(m.paneHeader(paneBranches, extra))
	b.WriteString("\n")
	height--

	if !m.isGitRepo {
		b.WriteString(dimStyle.Render("  Not a git repository"))
		return b.String()
	}

	if len(m.branches) == 0 {
		b.WriteString(dimStyle.Render("  No branches found"))
		return b.String()
	}

	start := 0
	vis := height
	if vis < 1 {
		vis = 1
	}
	if m.branchCursor >= start+vis {
		start = m.branchCursor - vis + 1
	}
	if m.branchCursor < start {
		start = m.branchCursor
	}
	end := start + vis
	if end > len(m.branches) {
		end = len(m.branches)
	}

	for i := start; i < end; i++ {
		branch := m.branches[i]
		isCursor := (i == m.branchCursor) && (m.focus == paneBranches)

		prefix := "  "
		if branch.IsCurrent {
			prefix = "* "
		}

		line := prefix + branch.Name

		if isCursor && branch.IsCurrent {
			line = cursorSelectedStyle.Render(line)
		} else if isCursor {
			line = cursorStyle.Render(line)
		} else if branch.IsCurrent {
			line = branchCurrentStyle.Render(line)
		} else {
			line = normalStyle.Render(line)
		}

		lineLen := lipgloss.Width(line)
		if lineLen < width && isCursor {
			pad := strings.Repeat(" ", width-lineLen)
			if branch.IsCurrent {
				line += cursorSelectedStyle.Render(pad)
			} else {
				line += cursorStyle.Render(pad)
			}
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

func (m Model) infoView(width, height int) string {
	var b strings.Builder

	b.WriteString(m.paneHeader(paneInfo, ""))
	b.WriteString("\n")

	tf := fmt.Sprintf("  %s %s",
		versionLabelStyle.Render("Terraform:"),
		versionValueStyle.Render(m.versionInfo.Terraform))
	tg := fmt.Sprintf("  %s %s",
		versionLabelStyle.Render("Terragrunt:"),
		versionValueStyle.Render(m.versionInfo.Terragrunt))

	b.WriteString(tf)
	if height > 2 {
		b.WriteString("\n")
		b.WriteString(tg)
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// [5] File View pane (with HCL syntax highlighting)
// ---------------------------------------------------------------------------

func (m Model) fileViewPane(width, height int) string {
	var b strings.Builder

	extra := ""
	if m.filePath != "" {
		extra = dimStyle.Render(m.filePath)
	}
	b.WriteString(m.paneHeader(paneFileView, extra))
	b.WriteString("\n")
	height--

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

	yankLo, yankHi := -1, -1
	if m.yankStart >= 0 {
		yankLo = m.yankStart
		yankHi = m.yankEnd
		if yankLo > yankHi {
			yankLo, yankHi = yankHi, yankLo
		}
	}

	lineNumW := len(fmt.Sprintf("%d", len(m.fileLines)))

	for i := start; i < end; i++ {
		num := fmt.Sprintf("%*d", lineNumW, i+1)
		content := m.fileLines[i]

		maxContentW := width - lineNumW - 2
		if maxContentW > 0 {
			runes := []rune(content)
			if len(runes) > maxContentW {
				content = string(runes[:maxContentW-3]) + "..."
			}
		}

		if i >= yankLo && i <= yankHi && yankLo >= 0 {
			b.WriteString(fmt.Sprintf("%s  %s", lineNumStyle.Render(num), yankHighlight.Render(content)))
		} else {
			// Apply HCL syntax highlighting.
			highlighted := highlightHCL(content)
			b.WriteString(fmt.Sprintf("%s  %s", lineNumStyle.Render(num), highlighted))
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
	// Hint line (lazygit style).
	b.WriteString(dimStyle.Render("  Focus this pane with ") + paneLabelStyle.Render("@"))
	b.WriteString("\n")
	height -= 2

	if len(m.cmdLogLines) == 0 {
		b.WriteString(dimStyle.Render("  No commands executed yet."))
		return b.String()
	}

	start := m.cmdLogScroll
	end := start + height
	if end > len(m.cmdLogLines) {
		end = len(m.cmdLogLines)
	}
	if start > end {
		start = end
	}

	for i := start; i < end; i++ {
		entry := m.cmdLogLines[i]
		text := entry.Text

		// Truncate for display.
		runes := []rune(text)
		if len(runes) > width && width > 3 {
			text = string(runes[:width-3]) + "..."
		}

		if entry.IsError {
			text = errorStyle.Render(text)
		} else if strings.HasPrefix(strings.TrimSpace(entry.Text), "━") {
			text = cmdLogHeaderStyle.Render(text)
		} else if strings.HasPrefix(strings.TrimSpace(entry.Text), "▶") {
			text = runningStyle.Render(text)
		} else if strings.HasPrefix(strings.TrimSpace(entry.Text), "✓") {
			text = successStyle.Render(text)
		} else if strings.HasPrefix(strings.TrimSpace(entry.Text), "✗") {
			text = errorStyle.Render(text)
		} else {
			// Apply some basic highlighting to terraform/terragrunt output.
			text = highlightCmdOutput(text)
		}

		b.WriteString(text)
		if i < end-1 {
			b.WriteString("\n")
		}
	}

	return b.String()
}

// highlightCmdOutput applies basic syntax-like coloring to command output lines.
func highlightCmdOutput(line string) string {
	trimmed := strings.TrimSpace(line)

	// Terraform plan-style coloring.
	if strings.HasPrefix(trimmed, "+") || strings.HasPrefix(trimmed, "# ") && strings.Contains(trimmed, "will be created") {
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
		keys := []string{
			helpKeyStyle.Render("Space") + " select",
			helpKeyStyle.Render("Enter") + " open",
			helpKeyStyle.Render("P") + " plan",
			helpKeyStyle.Render("A") + " apply",
			helpKeyStyle.Render("L") + " state list",
			helpKeyStyle.Render("1-5") + " panes",
			helpKeyStyle.Render("@") + " cmd log",
			helpKeyStyle.Render("?") + " help",
			helpKeyStyle.Render("q") + " quit",
		}
		left = strings.Join(keys, dimStyle.Render(" | "))
	}

	return lipgloss.NewStyle().
		Width(m.width).
		Padding(0, 1).
		Render(left)
}

// ---------------------------------------------------------------------------
// Help overlay — contextual per pane (like lazygit)
// ---------------------------------------------------------------------------

func (m Model) helpView() string {
	pName := paneName(m.focus)
	num := paneNumber(m.focus)
	var titleLabel string
	if num > 0 {
		titleLabel = fmt.Sprintf(" LazyTerra — [%d] %s", num, pName)
	} else {
		titleLabel = fmt.Sprintf(" LazyTerra — %s (@)", pName)
	}
	title := titleStyle.Render(titleLabel)

	var bindings [][]string

	// Pane-specific bindings.
	switch m.focus {
	case paneModules:
		bindings = [][]string{
			{"Modules", ""},
			{"j / Down", "Move cursor down"},
			{"k / Up", "Move cursor up"},
			{"g / Home", "Go to top"},
			{"G / End", "Go to bottom"},
			{"h", "Collapse directory / go to parent"},
			{"l", "Expand directory"},
			{"Enter", "Expand directory / open File View"},
			{"Space", "Toggle selection (stays on item)"},
			{"a", "Select all / deselect all"},
			{"/", "Search / filter modules"},
			{"", ""},
			{"Commands", ""},
			{"P (Shift+p)", "terragrunt plan"},
			{"A (Shift+a)", "terragrunt apply"},
			{"V (Shift+v)", "terragrunt validate"},
			{"i", "terragrunt init"},
			{"I (Shift+i)", "terragrunt init --reconfigure"},
			{"D (Shift+d)", "terragrunt destroy (confirm)"},
			{"O (Shift+o)", "terragrunt output"},
			{"L (Shift+l)", "terragrunt state list (cursor)"},
		}
	case paneDeps:
		bindings = [][]string{
			{"Dependencies", ""},
			{"", "(read-only — shows deps for cursor module)"},
		}
	case paneBranches:
		bindings = [][]string{
			{"Branches", ""},
			{"j / Down", "Move cursor down"},
			{"k / Up", "Move cursor up"},
			{"Enter / Space", "Checkout selected branch"},
		}
	case paneInfo:
		bindings = [][]string{
			{"Info", ""},
			{"", "(shows terraform & terragrunt versions)"},
		}
	case paneFileView:
		bindings = [][]string{
			{"File View", ""},
			{"j / Down", "Scroll down"},
			{"k / Up", "Scroll up"},
			{"g / Home", "Go to top"},
			{"G / End", "Go to bottom"},
			{"PgUp / PgDn", "Page scroll"},
			{"v", "Start visual line selection"},
			{"y", "Yank (copy) selected lines"},
			{"Esc", "Cancel visual selection"},
		}
	case paneCmdLog:
		bindings = [][]string{
			{"Command Log", ""},
			{"j / Down", "Scroll down"},
			{"k / Up", "Scroll up"},
			{"g / Home", "Go to top"},
			{"G / End", "Go to bottom"},
			{"PgUp / PgDn", "Page scroll"},
		}
	}

	// Always add global bindings.
	bindings = append(bindings, []string{"", ""})
	bindings = append(bindings, [][]string{
		{"Global", ""},
		{"1", "[1] Modules"},
		{"2", "[2] Dependencies"},
		{"3", "[3] Branches"},
		{"4", "[4] Info"},
		{"5", "[5] File View"},
		{"@", "Command Log"},
		{"Tab", "Cycle to next pane"},
		{"Esc", "Cancel command / clear search"},
		{"?", "Toggle this help"},
		{"q / Ctrl+C", "Quit"},
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
			// Description-only line (like a note).
			b.WriteString("  " + dimStyle.Render(pair[1]) + "\n")
			continue
		}
		b.WriteString(fmt.Sprintf("  %s  %s\n",
			helpKeyStyle.Width(20).Render(pair[0]),
			helpDescStyle.Render(pair[1]),
		))
	}

	b.WriteString("\n" + dimStyle.Render("Press any key to close"))

	content := borderStyle.
		Width(64).
		Padding(1, 2).
		Render(b.String())

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		lipgloss.JoinVertical(lipgloss.Center, title, content))
}

// ---------------------------------------------------------------------------
// Confirm overlay
// ---------------------------------------------------------------------------

func (m Model) confirmView() string {
	dirs, _ := m.selectedModuleDirs()
	msg := fmt.Sprintf("Are you sure you want to run terragrunt %s on %d module(s)?\n\n", m.confirmCmd.Name, len(dirs))
	for _, d := range dirs {
		msg += fmt.Sprintf("  - %s\n", d)
	}
	msg += "\nPress 'y' to confirm, any other key to cancel."

	content := confirmStyle.Render(msg)
	box := borderStyle.
		Width(60).
		Padding(1, 2).
		BorderForeground(lipgloss.Color("#F38BA8")).
		Render(content)

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nicoalimin/devbox/internal/api"
	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
)

// Pane represents which pane has focus
type Pane int

const (
	JobsPane Pane = iota
	LogsPane
	IntegrationsPane
)

// Model represents the TUI state
type Model struct {
	cfg             *config.Config
	database        *db.DB
	width           int
	height          int
	focusedPane     Pane
	logsViewport    viewport.Model
	jobsViewport    viewport.Model
	errorsViewport  viewport.Model
	lastUpdate      time.Time
	currentJob      *db.Job
	recentJobs      []*db.Job
	blockedJobs     []*db.Job
	recentLogs      []*db.JobLog
	selectedJobIdx  int
	quitting        bool
	ready           bool
}

type tickMsg time.Time

// NewModel creates a new TUI model
func NewModel(cfg *config.Config, database *db.DB) Model {
	return Model{
		cfg:            cfg,
		database:       database,
		focusedPane:    LogsPane,
		lastUpdate:     time.Now(),
		selectedJobIdx: 0,
	}
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.refreshData(),
		tickCmd(),
	)
}

// Update handles messages
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			return m, tea.Quit

		case "tab":
			// Cycle through panes
			m.focusedPane = (m.focusedPane + 1) % 3
			return m, nil

		case "shift+tab":
			// Cycle backward through panes
			m.focusedPane = (m.focusedPane + 2) % 3
			return m, nil

		case "r":
			cmds = append(cmds, m.refreshData())
			return m, tea.Batch(cmds...)

		case "j", "down":
			// Navigate down in focused pane
			switch m.focusedPane {
			case JobsPane:
				if m.selectedJobIdx < len(m.recentJobs)-1 {
					m.selectedJobIdx++
				}
			case LogsPane:
				m.logsViewport.LineDown(1)
			}
			return m, nil

		case "k", "up":
			// Navigate up in focused pane
			switch m.focusedPane {
			case JobsPane:
				if m.selectedJobIdx > 0 {
					m.selectedJobIdx--
				}
			case LogsPane:
				m.logsViewport.LineUp(1)
			}
			return m, nil

		case "g":
			// Go to top in focused pane
			switch m.focusedPane {
			case JobsPane:
				m.selectedJobIdx = 0
			case LogsPane:
				m.logsViewport.GotoTop()
			}
			return m, nil

		case "G":
			// Go to bottom in focused pane
			switch m.focusedPane {
			case JobsPane:
				if len(m.recentJobs) > 0 {
					m.selectedJobIdx = len(m.recentJobs) - 1
				}
			case LogsPane:
				m.logsViewport.GotoBottom()
			}
			return m, nil

		case "enter":
			// Show logs for selected job
			if m.focusedPane == JobsPane && m.selectedJobIdx < len(m.recentJobs) {
				selectedJob := m.recentJobs[m.selectedJobIdx]
				logs, _ := m.database.GetLogs(selectedJob.ID, 100)
				m.recentLogs = logs
				m.logsViewport.SetContent(m.renderLogsContent())
				m.focusedPane = LogsPane
			}
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		if !m.ready {
			// Initialize viewports with proper sizes
			m.logsViewport = viewport.New(msg.Width-32, msg.Height-6)
			m.jobsViewport = viewport.New(28, msg.Height/2-4)
			m.errorsViewport = viewport.New(28, msg.Height/2-4)
			m.ready = true
		} else {
			// Update viewport sizes on resize
			m.logsViewport.Width = msg.Width - 32
			m.logsViewport.Height = msg.Height - 6
			m.jobsViewport.Width = 28
			m.jobsViewport.Height = msg.Height/2 - 4
			m.errorsViewport.Width = 28
			m.errorsViewport.Height = msg.Height/2 - 4
		}

		return m, nil

	case tickMsg:
		m.lastUpdate = time.Time(msg)
		cmds = append(cmds, m.refreshData())
		cmds = append(cmds, tickCmd())
		return m, tea.Batch(cmds...)

	case dataRefreshMsg:
		m.currentJob = msg.currentJob
		m.recentJobs = msg.recentJobs
		m.blockedJobs = msg.blockedJobs
		m.recentLogs = msg.recentLogs

		// Update viewport contents
		if m.ready {
			m.logsViewport.SetContent(m.renderLogsContent())
			m.jobsViewport.SetContent(m.renderJobsContent())
			m.errorsViewport.SetContent(m.renderErrorsContent())
			
			// Auto-scroll logs to bottom
			m.logsViewport.GotoBottom()
		}
		return m, nil
	}

	// Update active viewport
	if m.ready {
		switch m.focusedPane {
		case LogsPane:
			m.logsViewport, cmd = m.logsViewport.Update(msg)
			cmds = append(cmds, cmd)
		case JobsPane:
			m.jobsViewport, cmd = m.jobsViewport.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

// View renders the full-screen TUI
func (m Model) View() string {
	if m.quitting {
		return ""
	}

	if !m.ready {
		return "Initializing..."
	}

	// Build the full-screen layout
	var s strings.Builder

	// Header
	s.WriteString(m.renderHeader())
	s.WriteString("\n")

	// Main content area: sidebar + logs
	contentArea := m.renderMainContent()
	s.WriteString(contentArea)
	s.WriteString("\n")

	// Footer
	s.WriteString(m.renderFooter())

	return s.String()
}

// renderHeader renders the top status bar
func (m Model) renderHeader() string {
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("15")).
		Background(lipgloss.Color("62")).
		Padding(0, 1).
		Width(m.width)

	var status string
	var statusColor lipgloss.Color

	if m.currentJob != nil {
		status = "BUSY"
		statusColor = lipgloss.Color("214")
	} else {
		status = "IDLE"
		statusColor = lipgloss.Color("35")
	}

	statusBadge := lipgloss.NewStyle().
		Bold(true).
		Foreground(statusColor).
		Render(status)

	var headerText string
	if m.currentJob != nil {
		elapsed := formatDuration(time.Since(m.currentJob.CreatedAt))
		headerText = fmt.Sprintf("devboxd v%s │ %s │ Job: %s (%s) │ %s",
			api.Version,
			statusBadge,
			m.currentJob.LinearIssueID,
			m.currentJob.State,
			elapsed)
	} else {
		headerText = fmt.Sprintf("devboxd v%s │ %s │ Updated: %s",
			api.Version,
			statusBadge,
			m.lastUpdate.Format("15:04:05"))
	}

	return headerStyle.Render(headerText)
}

// renderMainContent renders the main content area with sidebar and logs
func (m Model) renderMainContent() string {
	// Sidebar (left): Jobs + Errors + Integrations
	sidebar := m.renderSidebar()

	// Main pane (right): Logs
	mainPane := m.renderLogsPane()

	// Combine horizontally
	return lipgloss.JoinHorizontal(
		lipgloss.Top,
		sidebar,
		mainPane,
	)
}

// renderSidebar renders the left sidebar with jobs, errors, integrations
func (m Model) renderSidebar() string {
	sidebarWidth := 30

	// Jobs section
	jobsStyle := lipgloss.NewStyle().
		Width(sidebarWidth).
		Height(m.height/2 - 3).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(m.getBorderColor(JobsPane))

	jobsTitle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("213")).
		Render(fmt.Sprintf("JOBS (%d)", len(m.recentJobs)))

	jobsContent := m.renderJobsContent()
	m.jobsViewport.SetContent(jobsContent)
	
	jobs := lipgloss.JoinVertical(
		lipgloss.Left,
		jobsTitle,
		m.jobsViewport.View(),
	)

	// Errors section
	errorsStyle := lipgloss.NewStyle().
		Width(sidebarWidth).
		Height(m.height/2 - 3).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("241"))

	errorsTitle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("196")).
		Render(fmt.Sprintf("ERRORS (%d)", len(m.blockedJobs)))

	errorsContent := m.renderErrorsContent()
	m.errorsViewport.SetContent(errorsContent)

	errors := lipgloss.JoinVertical(
		lipgloss.Left,
		errorsTitle,
		m.errorsViewport.View(),
	)

	// Integrations section
	integrationsBar := m.renderIntegrationsBar()

	// Stack vertically
	sidebar := lipgloss.JoinVertical(
		lipgloss.Left,
		jobsStyle.Render(jobs),
		errorsStyle.Render(errors),
		integrationsBar,
	)

	return sidebar
}

// renderJobsContent renders the jobs list content
func (m Model) renderJobsContent() string {
	var s strings.Builder

	if len(m.recentJobs) == 0 {
		s.WriteString(dimStyle.Render("No jobs yet"))
		return s.String()
	}

	for i, job := range m.recentJobs {
		if i >= 20 { // Limit displayed jobs
			break
		}

		var prefix string
		if i == m.selectedJobIdx && m.focusedPane == JobsPane {
			prefix = highlightStyle.Render("▸ ")
		} else {
			prefix = "  "
		}

		// State color
		var stateStyle lipgloss.Style
		switch job.State {
		case db.StateDone:
			stateStyle = successStyle
		case db.StateFailed, db.StateCancelled:
			stateStyle = errorStyle
		case db.StateBlocked:
			stateStyle = warningStyle
		default:
			stateStyle = activeStyle
		}

		state := stateStyle.Render(string(job.State))
		
		// Truncate job ID for display
		displayID := job.LinearIssueID
		if len(displayID) > 12 {
			displayID = displayID[:12]
		}

		s.WriteString(fmt.Sprintf("%s%s %s\n", prefix, displayID, state))
	}

	return s.String()
}

// renderErrorsContent renders blocked/failed jobs
func (m Model) renderErrorsContent() string {
	var s strings.Builder

	if len(m.blockedJobs) == 0 {
		s.WriteString(dimStyle.Render("No blockers"))
		return s.String()
	}

	for i, job := range m.blockedJobs {
		if i >= 5 { // Show max 5 blockers
			break
		}
		
		s.WriteString(errorStyle.Render("• ") + job.LinearIssueID + "\n")
		if job.BlockerReason != "" {
			reason := job.BlockerReason
			if len(reason) > 22 {
				reason = reason[:22] + "…"
			}
			s.WriteString(dimStyle.Render("  " + reason) + "\n")
		}
	}

	return s.String()
}

// renderIntegrationsBar renders the integrations health bar
func (m Model) renderIntegrationsBar() string {
	barStyle := lipgloss.NewStyle().
		Width(30).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(m.getBorderColor(IntegrationsPane)).
		Padding(0, 1)

	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("86")).
		Render("INTEGRATIONS")

	health := successStyle.Render("✓ Linear") + " " +
		successStyle.Render("✓ GitHub") + " " +
		successStyle.Render("✓ OpenCode")

	repos := dimStyle.Render(fmt.Sprintf("%d repos configured", len(m.cfg.Repos)))

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		health,
		repos,
	)

	return barStyle.Render(content)
}

// renderLogsPane renders the main logs pane
func (m Model) renderLogsPane() string {
	logsStyle := lipgloss.NewStyle().
		Width(m.width - 32).
		Height(m.height - 4).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(m.getBorderColor(LogsPane))

	title := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("117")).
		Render("LIVE LOGS")

	subtitle := dimStyle.Render("(streaming from current job)")

	header := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		subtitle,
	)

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		m.logsViewport.View(),
	)

	return logsStyle.Render(content)
}

// renderLogsContent renders the scrollable logs content
func (m Model) renderLogsContent() string {
	var s strings.Builder

	if len(m.recentLogs) == 0 {
		s.WriteString(dimStyle.Render("No logs available"))
		return s.String()
	}

	for _, log := range m.recentLogs {
		timestamp := dimStyle.Render(log.Timestamp.Format("15:04:05"))

		var level string
		switch strings.ToLower(log.Level) {
		case "error":
			level = errorStyle.Render("[ERROR]")
		case "warn":
			level = warningStyle.Render("[WARN] ")
		default:
			level = infoStyle.Render("[INFO] ")
		}

		s.WriteString(fmt.Sprintf("%s %s %s\n", timestamp, level, log.Message))
	}

	return s.String()
}

// renderFooter renders the bottom keybindings bar
func (m Model) renderFooter() string {
	footerStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		Background(lipgloss.Color("235")).
		Padding(0, 1).
		Width(m.width)

	var focusedPaneName string
	switch m.focusedPane {
	case JobsPane:
		focusedPaneName = "Jobs"
	case LogsPane:
		focusedPaneName = "Logs"
	case IntegrationsPane:
		focusedPaneName = "Integrations"
	}

	keybindings := fmt.Sprintf("Focus: %s │ Tab: switch pane │ ↑↓/jk: navigate │ Enter: view logs │ r: refresh │ q: quit",
		highlightStyle.Render(focusedPaneName))

	return footerStyle.Render(keybindings)
}

// getBorderColor returns the border color for a pane based on focus
func (m Model) getBorderColor(pane Pane) lipgloss.Color {
	if m.focusedPane == pane {
		return lipgloss.Color("170") // Highlight color
	}
	return lipgloss.Color("241") // Dim color
}

// refreshData refreshes data from the database
func (m Model) refreshData() tea.Cmd {
	return func() tea.Msg {
		currentJob, _ := m.database.GetCurrentJob()
		recentJobs, _ := m.database.ListJobs(50)
		blockedJobs, _ := m.database.GetBlockedJobs()

		// Get logs from the global log buffer (live daemon logs)
		// This includes HTTP access logs, job logs, etc.
		var recentLogs []*db.JobLog
		if globalLogBuffer != nil {
			bufferLogs := globalLogBuffer.GetRecent(200)
			for _, entry := range bufferLogs {
				recentLogs = append(recentLogs, &db.JobLog{
					Timestamp: entry.Timestamp,
					Level:     entry.Level,
					Message:   entry.Message,
				})
			}
		} else {
			// Fallback to database logs if buffer not initialized
			if currentJob != nil {
				recentLogs, _ = m.database.GetLogs(currentJob.ID, 200)
			} else if len(recentJobs) > 0 {
				recentLogs, _ = m.database.GetLogs(recentJobs[0].ID, 200)
			}
		}

		return dataRefreshMsg{
			currentJob:  currentJob,
			recentJobs:  recentJobs,
			blockedJobs: blockedJobs,
			recentLogs:  recentLogs,
		}
	}
}

// dataRefreshMsg carries refreshed data
type dataRefreshMsg struct {
	currentJob  *db.Job
	recentJobs  []*db.Job
	blockedJobs []*db.Job
	recentLogs  []*db.JobLog
}

// tickCmd creates a tick command for auto-refresh
func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// formatDuration formats a duration in a human-readable way
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// Styles
var (
	highlightStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("170")).Bold(true)
	dimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	successStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("35"))
	errorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	warningStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	activeStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("33"))
	infoStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
)

// Run starts the TUI
func Run(cfg *config.Config, database *db.DB) error {
	m := NewModel(cfg, database)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

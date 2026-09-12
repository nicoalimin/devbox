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

// Tab represents a TUI tab
type Tab int

const (
	StatusTab Tab = iota
	HistoryTab
	ErrorsTab
	IntegrationsTab
	LogsTab
)

// Model represents the TUI state
type Model struct {
	cfg          *config.Config
	database     *db.DB
	activeTab    Tab
	viewport     viewport.Model
	width        int
	height       int
	lastUpdate   time.Time
	currentJob   *db.Job
	recentJobs   []*db.Job
	blockedJobs  []*db.Job
	recentLogs   []*db.JobLog
	quitting     bool
}

type tickMsg time.Time

// NewModel creates a new TUI model
func NewModel(cfg *config.Config, database *db.DB) Model {
	vp := viewport.New(0, 0)
	vp.Style = lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("62"))

	return Model{
		cfg:        cfg,
		database:   database,
		activeTab:  StatusTab,
		viewport:   vp,
		lastUpdate: time.Now(),
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

		case "1":
			m.activeTab = StatusTab
			m.viewport.SetContent(m.renderStatusTab())
			return m, nil

		case "2":
			m.activeTab = HistoryTab
			m.viewport.SetContent(m.renderHistoryTab())
			return m, nil

		case "3":
			m.activeTab = ErrorsTab
			m.viewport.SetContent(m.renderErrorsTab())
			return m, nil

		case "4":
			m.activeTab = IntegrationsTab
			m.viewport.SetContent(m.renderIntegrationsTab())
			return m, nil

		case "5":
			m.activeTab = LogsTab
			m.viewport.SetContent(m.renderLogsTab())
			return m, nil

		case "r":
			cmds = append(cmds, m.refreshData())
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		headerHeight := 4
		footerHeight := 2
		m.viewport.Width = msg.Width - 2
		m.viewport.Height = msg.Height - headerHeight - footerHeight

	case tickMsg:
		m.lastUpdate = time.Time(msg)
		cmds = append(cmds, m.refreshData())
		cmds = append(cmds, tickCmd())

	case dataRefreshMsg:
		m.currentJob = msg.currentJob
		m.recentJobs = msg.recentJobs
		m.blockedJobs = msg.blockedJobs
		m.recentLogs = msg.recentLogs
		
		// Update viewport content
		var content string
		switch m.activeTab {
		case StatusTab:
			content = m.renderStatusTab()
		case HistoryTab:
			content = m.renderHistoryTab()
		case ErrorsTab:
			content = m.renderErrorsTab()
		case IntegrationsTab:
			content = m.renderIntegrationsTab()
		case LogsTab:
			content = m.renderLogsTab()
		}
		m.viewport.SetContent(content)
	}

	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// View renders the TUI
func (m Model) View() string {
	if m.quitting {
		return ""
	}

	var s strings.Builder

	// Header
	s.WriteString(m.renderHeader())
	s.WriteString("\n")

	// Tab bar
	s.WriteString(m.renderTabBar())
	s.WriteString("\n\n")

	// Content viewport
	s.WriteString(m.viewport.View())
	s.WriteString("\n")

	// Footer
	s.WriteString(m.renderFooter())

	return s.String()
}

// renderHeader renders the header
func (m Model) renderHeader() string {
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("205")).
		MarginLeft(2)

	statusStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		MarginLeft(2)

	status := "IDLE"
	statusColor := lipgloss.Color("35")
	if m.currentJob != nil {
		status = "BUSY"
		statusColor = lipgloss.Color("214")
	}

	statusBadge := lipgloss.NewStyle().
		Bold(true).
		Foreground(statusColor).
		Render(status)

	title := titleStyle.Render("devboxd v" + api.Version)
	statusText := statusStyle.Render(fmt.Sprintf(" [%s] Updated: %s",
		statusBadge,
		m.lastUpdate.Format("15:04:05")))

	return title + statusText
}

// renderTabBar renders the tab navigation bar
func (m Model) renderTabBar() string {
	activeStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("205")).
		Background(lipgloss.Color("235")).
		Padding(0, 1)

	inactiveStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		Padding(0, 1)

	tabs := []string{"1:Status", "2:History", "3:Errors", "4:Integrations", "5:Logs"}
	var rendered []string

	for i, tab := range tabs {
		if Tab(i) == m.activeTab {
			rendered = append(rendered, activeStyle.Render(tab))
		} else {
			rendered = append(rendered, inactiveStyle.Render(tab))
		}
	}

	return "  " + strings.Join(rendered, " ")
}

// renderFooter renders the footer
func (m Model) renderFooter() string {
	helpStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		MarginLeft(2)

	return helpStyle.Render("1-5: Switch tabs • r: Refresh • q/Ctrl+C: Quit")
}

// renderStatusTab renders the status tab
func (m Model) renderStatusTab() string {
	var s strings.Builder

	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	valueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))

	s.WriteString(labelStyle.Render("📊 Current Status\n\n"))

	// Version
	s.WriteString(valueStyle.Render(fmt.Sprintf("Version:        %s\n", api.Version)))

	// Current job
	if m.currentJob != nil {
		s.WriteString(valueStyle.Render(fmt.Sprintf("Status:         BUSY\n")))
		s.WriteString(valueStyle.Render(fmt.Sprintf("Current Job ID: %s\n", m.currentJob.ID)))
		s.WriteString(valueStyle.Render(fmt.Sprintf("Linear Issue:   %s\n", m.currentJob.LinearIssueID)))
		s.WriteString(valueStyle.Render(fmt.Sprintf("State:          %s\n", m.currentJob.State)))
		if m.currentJob.RepoPath != "" {
			s.WriteString(valueStyle.Render(fmt.Sprintf("Repository:     %s\n", m.currentJob.RepoPath)))
		}
		if m.currentJob.BranchName != "" {
			s.WriteString(valueStyle.Render(fmt.Sprintf("Branch:         %s\n", m.currentJob.BranchName)))
		}
		if m.currentJob.PRURL != "" {
			s.WriteString(valueStyle.Render(fmt.Sprintf("PR URL:         %s\n", m.currentJob.PRURL)))
		}
		elapsed := time.Since(m.currentJob.CreatedAt)
		s.WriteString(valueStyle.Render(fmt.Sprintf("Running for:    %s\n", formatDuration(elapsed))))
	} else {
		s.WriteString(valueStyle.Render("Status:         IDLE\n"))
		s.WriteString(valueStyle.Render("Current Job:    None\n"))
	}

	// Summary stats
	s.WriteString("\n")
	s.WriteString(labelStyle.Render("📈 Statistics\n\n"))
	s.WriteString(valueStyle.Render(fmt.Sprintf("Recent Jobs:    %d\n", len(m.recentJobs))))
	s.WriteString(valueStyle.Render(fmt.Sprintf("Blocked Jobs:   %d\n", len(m.blockedJobs))))

	return s.String()
}

// renderHistoryTab renders the job history tab
func (m Model) renderHistoryTab() string {
	var s strings.Builder

	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("241"))

	s.WriteString(labelStyle.Render("📜 Job History\n\n"))

	if len(m.recentJobs) == 0 {
		s.WriteString("No jobs found.\n")
		return s.String()
	}

	// Table header
	s.WriteString(headerStyle.Render(fmt.Sprintf("%-36s %-15s %-12s %-20s\n",
		"JOB ID", "LINEAR ISSUE", "STATE", "CREATED")))
	s.WriteString(strings.Repeat("─", 100) + "\n")

	// Table rows
	for _, job := range m.recentJobs {
		stateStyle := lipgloss.NewStyle()
		switch job.State {
		case db.StateDone:
			stateStyle = stateStyle.Foreground(lipgloss.Color("35"))
		case db.StateFailed, db.StateCancelled:
			stateStyle = stateStyle.Foreground(lipgloss.Color("196"))
		case db.StateBlocked:
			stateStyle = stateStyle.Foreground(lipgloss.Color("214"))
		default:
			stateStyle = stateStyle.Foreground(lipgloss.Color("33"))
		}

		s.WriteString(fmt.Sprintf("%-36s %-15s %-12s %-20s\n",
			job.ID[:36],
			job.LinearIssueID,
			stateStyle.Render(string(job.State)),
			job.CreatedAt.Format("2006-01-02 15:04:05")))

		// Show additional details for recent jobs
		if job.PRURL != "" {
			s.WriteString(fmt.Sprintf("  PR: %s\n", job.PRURL))
		}
		if job.BlockerReason != "" {
			s.WriteString(fmt.Sprintf("  Blocker: %s\n", job.BlockerReason))
		}
		s.WriteString("\n")
	}

	return s.String()
}

// renderErrorsTab renders the errors/blockers tab
func (m Model) renderErrorsTab() string {
	var s strings.Builder

	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	infoStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))

	s.WriteString(labelStyle.Render("⚠️  Errors & Blockers\n\n"))

	if len(m.blockedJobs) == 0 {
		s.WriteString("No blocked jobs.\n")
		return s.String()
	}

	for i, job := range m.blockedJobs {
		s.WriteString(errorStyle.Render(fmt.Sprintf("Job %d:\n", i+1)))
		s.WriteString(infoStyle.Render(fmt.Sprintf("  ID:           %s\n", job.ID)))
		s.WriteString(infoStyle.Render(fmt.Sprintf("  Linear Issue: %s\n", job.LinearIssueID)))
		s.WriteString(infoStyle.Render(fmt.Sprintf("  State:        %s\n", job.State)))
		s.WriteString(infoStyle.Render(fmt.Sprintf("  Blocker:      %s\n", job.BlockerReason)))
		s.WriteString(infoStyle.Render(fmt.Sprintf("  Created:      %s\n", job.CreatedAt.Format("2006-01-02 15:04:05"))))
		if i < len(m.blockedJobs)-1 {
			s.WriteString("\n")
		}
	}

	return s.String()
}

// renderIntegrationsTab renders the integrations health tab
func (m Model) renderIntegrationsTab() string {
	var s strings.Builder

	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	healthyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("35"))
	infoStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))

	s.WriteString(labelStyle.Render("🔧 Integrations\n\n"))

	// Linear
	s.WriteString(healthyStyle.Render("✓") + " Linear\n")
	s.WriteString(infoStyle.Render(fmt.Sprintf("  API Key:      %s***\n", m.cfg.Linear.APIKey[:8])))
	if m.cfg.Linear.AssigneeID != "" {
		s.WriteString(infoStyle.Render(fmt.Sprintf("  Assignee ID:  %s\n", m.cfg.Linear.AssigneeID)))
	}
	s.WriteString("\n")

	// OpenCode
	s.WriteString(healthyStyle.Render("✓") + " OpenCode\n")
	s.WriteString(infoStyle.Render(fmt.Sprintf("  Base URL:     %s\n", m.cfg.OpenCode.BaseURL)))
	s.WriteString(infoStyle.Render(fmt.Sprintf("  Timeout:      %s\n", m.cfg.OpenCode.Timeout)))
	s.WriteString("\n")

	// GitHub
	s.WriteString(healthyStyle.Render("✓") + " GitHub\n")
	s.WriteString(infoStyle.Render(fmt.Sprintf("  Base Branch:  %s\n", m.cfg.GitHub.DefaultBaseBranch)))
	s.WriteString("\n")

	// Repositories
	s.WriteString(labelStyle.Render("📁 Configured Repositories\n\n"))
	for i, repo := range m.cfg.Repos {
		s.WriteString(infoStyle.Render(fmt.Sprintf("%d. %s\n", i+1, repo.Repo.Path)))
		if repo.Match.Team != "" {
			s.WriteString(infoStyle.Render(fmt.Sprintf("   Match: Team=%s\n", repo.Match.Team)))
		}
		if repo.Match.Project != "" {
			s.WriteString(infoStyle.Render(fmt.Sprintf("   Match: Project=%s\n", repo.Match.Project)))
		}
		if repo.Match.Label != "" {
			s.WriteString(infoStyle.Render(fmt.Sprintf("   Match: Label=%s\n", repo.Match.Label)))
		}
		s.WriteString(infoStyle.Render(fmt.Sprintf("   Base Branch: %s\n", repo.Repo.BaseBranch)))
		if i < len(m.cfg.Repos)-1 {
			s.WriteString("\n")
		}
	}

	return s.String()
}

// renderLogsTab renders the logs tab
func (m Model) renderLogsTab() string {
	var s strings.Builder

	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	timeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	infoStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))

	s.WriteString(labelStyle.Render("📝 Recent Logs\n\n"))

	if len(m.recentLogs) == 0 {
		s.WriteString("No recent logs.\n")
		return s.String()
	}

	for _, log := range m.recentLogs {
		timestamp := timeStyle.Render(log.Timestamp.Format("15:04:05"))
		
		var level string
		switch strings.ToLower(log.Level) {
		case "error":
			level = errorStyle.Render("[ERROR]")
		case "warn":
			level = warnStyle.Render("[WARN] ")
		default:
			level = infoStyle.Render("[INFO] ")
		}

		message := infoStyle.Render(log.Message)
		s.WriteString(fmt.Sprintf("%s %s %s\n", timestamp, level, message))
	}

	return s.String()
}

// refreshData refreshes data from the database
func (m Model) refreshData() tea.Cmd {
	return func() tea.Msg {
		currentJob, _ := m.database.GetCurrentJob()
		recentJobs, _ := m.database.ListJobs(20)
		blockedJobs, _ := m.database.GetBlockedJobs()
		
		var recentLogs []*db.JobLog
		if currentJob != nil {
			recentLogs, _ = m.database.GetLogs(currentJob.ID, 50)
		} else if len(recentJobs) > 0 {
			recentLogs, _ = m.database.GetLogs(recentJobs[0].ID, 50)
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

// tickCmd creates a tick command
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
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// Run starts the TUI
func Run(cfg *config.Config, database *db.DB) error {
	m := NewModel(cfg, database)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

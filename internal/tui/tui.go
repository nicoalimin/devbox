package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nicoalimin/devbox/internal/api"
	"github.com/nicoalimin/devbox/internal/buildinfo"
	"github.com/nicoalimin/devbox/internal/config"
	"github.com/nicoalimin/devbox/internal/db"
	"github.com/nicoalimin/devbox/internal/linear"
)

// Pane represents which pane has focus
type Pane int

const (
	JobsPane Pane = iota
	TicketInfoPane
	ServerLogsPane
	JobLogsPane
	IntegrationsPane
)

type issueGetter interface {
	GetIssue(string) (*linear.Issue, error)
}

// Model represents the TUI state
type Model struct {
	cfg                *config.Config
	database           *db.DB
	width              int
	height             int
	focusedPane        Pane
	serverLogsViewport viewport.Model
	jobLogsViewport    viewport.Model
	jobsViewport       viewport.Model
	ticketViewport     viewport.Model
	lastUpdate         time.Time
	startedAt          time.Time
	currentJob         *db.Job
	recentJobs         []*db.Job
	serverLogs         []*db.JobLog
	jobLogs            []*db.JobLog
	selectedJobIdx     int
	issueClient        issueGetter
	ticketIssue        *linear.Issue
	ticketError        string
	ticketLoadingFor   string
	ticketLoadedFor    string
	quitting           bool
	ready              bool
}

type tickMsg time.Time

// NewModel creates a new TUI model
func NewModel(cfg *config.Config, database *db.DB) Model {
	return NewModelWithStartedAt(cfg, database, time.Now())
}

// NewModelWithStartedAt creates a TUI model tied to the current daemon
// instance, so the displayed uptime resets when the daemon restarts.
func NewModelWithStartedAt(cfg *config.Config, database *db.DB, startedAt time.Time) Model {
	now := time.Now()
	return Model{
		cfg:            cfg,
		database:       database,
		focusedPane:    ServerLogsPane,
		lastUpdate:     now,
		startedAt:      startedAt,
		selectedJobIdx: -1,
		issueClient:    linear.NewClient(cfg.Linear.APIKey),
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
			m.focusedPane = (m.focusedPane + 1) % 5
			return m, nil

		case "shift+tab":
			// Cycle backward through panes
			m.focusedPane = (m.focusedPane + 4) % 5
			return m, nil

		case "r":
			m.ticketLoadedFor = ""
			cmds = append(cmds, m.refreshData())
			return m, tea.Batch(cmds...)

		case "j", "down":
			// Navigate down in focused pane
			switch m.focusedPane {
			case JobsPane:
				if m.selectedJobIdx < len(m.recentJobs)-1 {
					m.selectedJobIdx++
					// Scroll to bottom immediately when selecting a different job
					if m.ready {
						m.jobLogsViewport.GotoBottom()
					}
					m.ticketViewport.GotoTop()
					cmds = append(cmds, m.refreshJobLogs(), m.startTicketRefresh())
				}
			case TicketInfoPane:
				m.ticketViewport.LineDown(1)
			case ServerLogsPane:
				m.serverLogsViewport.LineDown(1)
			case JobLogsPane:
				m.jobLogsViewport.LineDown(1)
			}
			return m, tea.Batch(cmds...)

		case "k", "up":
			// Navigate up in focused pane
			switch m.focusedPane {
			case JobsPane:
				if m.selectedJobIdx > 0 {
					m.selectedJobIdx--
					// Scroll to bottom immediately when selecting a different job
					if m.ready {
						m.jobLogsViewport.GotoBottom()
					}
					m.ticketViewport.GotoTop()
					cmds = append(cmds, m.refreshJobLogs(), m.startTicketRefresh())
				}
			case TicketInfoPane:
				m.ticketViewport.LineUp(1)
			case ServerLogsPane:
				m.serverLogsViewport.LineUp(1)
			case JobLogsPane:
				m.jobLogsViewport.LineUp(1)
			}
			return m, tea.Batch(cmds...)

		case "g":
			// Go to top in focused pane
			switch m.focusedPane {
			case JobsPane:
				m.selectedJobIdx = 0
				// Scroll to bottom immediately when selecting a different job
				if m.ready {
					m.jobLogsViewport.GotoBottom()
				}
				m.ticketViewport.GotoTop()
				cmds = append(cmds, m.refreshJobLogs(), m.startTicketRefresh())
			case TicketInfoPane:
				m.ticketViewport.GotoTop()
			case ServerLogsPane:
				m.serverLogsViewport.GotoTop()
			case JobLogsPane:
				m.jobLogsViewport.GotoTop()
			}
			return m, tea.Batch(cmds...)

		case "G":
			// Go to bottom in focused pane
			switch m.focusedPane {
			case JobsPane:
				if len(m.recentJobs) > 0 {
					m.selectedJobIdx = len(m.recentJobs) - 1
					// Scroll to bottom immediately when selecting a different job
					if m.ready {
						m.jobLogsViewport.GotoBottom()
					}
					m.ticketViewport.GotoTop()
					cmds = append(cmds, m.refreshJobLogs(), m.startTicketRefresh())
				}
			case TicketInfoPane:
				m.ticketViewport.GotoBottom()
			case ServerLogsPane:
				m.serverLogsViewport.GotoBottom()
			case JobLogsPane:
				m.jobLogsViewport.GotoBottom()
			}
			return m, tea.Batch(cmds...)

		case "enter":
			// Switch focus to job logs pane
			if m.focusedPane == JobsPane {
				m.focusedPane = JobLogsPane
				// Immediately scroll to bottom when entering job logs
				if m.ready {
					m.jobLogsViewport.GotoBottom()
				}
			}
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		// Calculate actual header and footer heights accounting for text wrapping.
		// This is critical: on narrow terminals (e.g. 80 cols), the footer can wrap
		// to 2+ lines, and the header may also wrap if showing a long job name.
		headerHeight := m.getHeaderHeight()
		footerHeight := m.getFooterHeight()

		// Calculate viewport heights to fit layout exactly:
		// Layout: Header (headerHeight) + newline (1) + Main Content + newline (1) + Footer (footerHeight) = height
		// So: Main Content must be exactly (height - headerHeight - footerHeight - 2)
		availableContentHeight := msg.Height - headerHeight - footerHeight - 2

		// Sidebar (stacked vertically):
		//   - Jobs section: title (1) + viewport content + borders (2) = viewport + 3
		//   - Ticket Information: title (1) + viewport content + borders (2) = viewport + 3
		//   - Integrations bar: 5 lines (3 content + 2 borders)
		//   Total sidebar = (jobsViewport + 3) + (ticketViewport + 3) + 5 = jobsViewport + ticketViewport + 11
		//
		// For sidebar to equal availableContentHeight:
		//   jobsViewport + ticketViewport + 11 = availableContentHeight
		//   jobsViewport + ticketViewport = availableContentHeight - 11
		//   Split equally: each = (availableContentHeight - 11) / 2
		//
		// Logs pane (split into job logs + server logs, stacked vertically):
		//   Each log section: title (1) + subtitle (1) + viewport + borders (2) = viewport + 4
		//   Total = (jobLogsViewport + 4) + (serverLogsViewport + 4) = availableContentHeight
		//   jobLogsViewport + serverLogsViewport = availableContentHeight - 8
		//   Split with 2:1 ratio (Job Logs taller): jobLogs = 2/3, serverLogs = 1/3

		sidebarWidth := 30

		// Jobs and ticket information split the available height, accounting for integrations bar (~5 lines)
		// Each section needs: title (1) + viewport + borders (2) = viewport + 3
		// Total: jobsViewport + ticketViewport + 6 + integrations (5) = availableContentHeight
		sidebarJobsTicket := availableContentHeight - 5 // 5 for integrations bar
		jobsHeight := (sidebarJobsTicket - 6) / 2       // -6 for titles (2) and borders (4) across both sections
		ticketHeight := (sidebarJobsTicket - 6) / 2

		// Split logs pane into two sections (job logs above, server logs beneath)
		// Each section renders as: title (1) + subtitle (1) + viewport + borders (2) = viewport + 4
		// Total: (jobLogsViewport + 4) + (serverLogsViewport + 4) = availableContentHeight
		// So: jobLogsViewport + serverLogsViewport = availableContentHeight - 8
		// Allocate 2:1 ratio (Job Logs get ~67%, Server Logs get ~33%)
		logsAvailable := availableContentHeight - 8
		jobLogsHeight := (logsAvailable * 2) / 3
		serverLogsHeight := logsAvailable - jobLogsHeight

		// Ensure minimum heights
		if jobsHeight < 3 {
			jobsHeight = 3
		}
		if ticketHeight < 3 {
			ticketHeight = 3
		}
		if serverLogsHeight < 3 {
			serverLogsHeight = 3
		}
		if jobLogsHeight < 3 {
			jobLogsHeight = 3
		}

		if !m.ready {
			// Initialize viewports with proper sizes
			logsWidth := msg.Width - sidebarWidth - 2
			m.serverLogsViewport = viewport.New(logsWidth, serverLogsHeight)
			m.jobLogsViewport = viewport.New(logsWidth, jobLogsHeight)
			m.jobsViewport = viewport.New(sidebarWidth-2, jobsHeight)
			m.ticketViewport = viewport.New(sidebarWidth-2, ticketHeight)
			m.ready = true
		} else {
			// Update viewport sizes on resize
			logsWidth := msg.Width - sidebarWidth - 2
			m.serverLogsViewport.Width = logsWidth
			m.serverLogsViewport.Height = serverLogsHeight
			m.jobLogsViewport.Width = logsWidth
			m.jobLogsViewport.Height = jobLogsHeight
			m.jobsViewport.Width = sidebarWidth - 2
			m.jobsViewport.Height = jobsHeight
			m.ticketViewport.Width = sidebarWidth - 2
			m.ticketViewport.Height = ticketHeight
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
		m.serverLogs = msg.serverLogs
		m.jobLogs = msg.jobLogs

		// Update viewport contents
		if m.ready {
			setViewportContent(m.serverLogsViewport.AtBottom(), &m.serverLogsViewport, m.renderServerLogsContent())
			setViewportContent(m.jobLogsViewport.AtBottom(), &m.jobLogsViewport, m.renderJobLogsContent())
			m.jobsViewport.SetContent(m.renderJobsContent())
			m.ticketViewport.SetContent(m.renderTicketContent())
		}

		target := m.ticketJob()
		if target == nil {
			m.ticketIssue = nil
			m.ticketError = ""
			m.ticketLoadingFor = ""
			m.ticketLoadedFor = ""
		} else if m.ticketLoadedFor != target.ID && m.ticketLoadingFor != target.ID {
			return m, m.startTicketRefresh()
		}
		return m, nil

	case jobLogsRefreshMsg:
		m.jobLogs = msg.jobLogs
		if m.ready {
			setViewportContent(m.jobLogsViewport.AtBottom(), &m.jobLogsViewport, m.renderJobLogsContent())
		}
		return m, nil

	case ticketRefreshMsg:
		if target := m.ticketJob(); target != nil && target.ID == msg.jobID {
			m.ticketIssue = msg.issue
			m.ticketError = msg.err
			m.ticketLoadingFor = ""
			m.ticketLoadedFor = msg.jobID
			if m.ready {
				m.ticketViewport.SetContent(m.renderTicketContent())
			}
		}
		return m, nil
	}

	// Update active viewport
	if m.ready {
		switch m.focusedPane {
		case ServerLogsPane:
			m.serverLogsViewport, cmd = m.serverLogsViewport.Update(msg)
			cmds = append(cmds, cmd)
		case JobLogsPane:
			m.jobLogsViewport, cmd = m.jobLogsViewport.Update(msg)
			cmds = append(cmds, cmd)
		case JobsPane:
			m.jobsViewport, cmd = m.jobsViewport.Update(msg)
			cmds = append(cmds, cmd)
		case TicketInfoPane:
			m.ticketViewport, cmd = m.ticketViewport.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

// setViewportContent updates dynamic content without stealing the reader's
// scroll position. A viewport follows new output only while it was already at
// the bottom; once the user scrolls up, refreshes preserve that offset.
func setViewportContent(wasAtBottom bool, target *viewport.Model, content string) {
	target.SetContent(content)
	if wasAtBottom {
		target.GotoBottom()
	}
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
		Padding(1, 1, 0, 1). // Top, Right, Bottom, Left padding (add top padding to prevent clipping)
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
	// Sidebar (left): Jobs + Ticket Information + Integrations
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

// renderSidebar renders the left sidebar with jobs, ticket information, integrations
func (m Model) renderSidebar() string {
	sidebarWidth := 30

	// Jobs section - height includes title (1) + viewport + borders (2)
	jobsStyle := lipgloss.NewStyle().
		Width(sidebarWidth).
		Height(m.jobsViewport.Height + 3).
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

	// Ticket Information section - height includes title (1) + viewport + borders (2)
	ticketStyle := lipgloss.NewStyle().
		Width(sidebarWidth).
		Height(m.ticketViewport.Height + 3).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(m.getBorderColor(TicketInfoPane))

	ticketTitle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("117")).
		Render("TICKET INFORMATION")

	ticketContent := m.renderTicketContent()
	m.ticketViewport.SetContent(ticketContent)

	ticket := lipgloss.JoinVertical(
		lipgloss.Left,
		ticketTitle,
		m.ticketViewport.View(),
	)

	// Integrations section
	integrationsBar := m.renderIntegrationsBar()

	// Stack vertically
	sidebar := lipgloss.JoinVertical(
		lipgloss.Left,
		jobsStyle.Render(jobs),
		ticketStyle.Render(ticket),
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
		case db.StateBlocked, db.StateStuck:
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

// renderTicketContent renders the selected Linear ticket's details.
func (m Model) renderTicketContent() string {
	target := m.ticketJob()
	if target == nil {
		return dimStyle.Render("No running task")
	}
	if m.ticketLoadingFor == target.ID {
		return dimStyle.Render("Loading " + target.LinearIssueID + "…")
	}
	if m.ticketError != "" {
		return warningStyle.Render(target.LinearIssueID) + "\n" +
			dimStyle.Render(wrapText("Unable to load ticket: "+m.ticketError, m.ticketViewport.Width))
	}
	issue := m.ticketIssue
	if issue == nil || issue.Identifier != target.LinearIssueID {
		return dimStyle.Render("Ticket details unavailable")
	}

	width := m.ticketViewport.Width
	var s strings.Builder
	s.WriteString(highlightStyle.Render(issue.Identifier) + "\n")
	s.WriteString(lipgloss.NewStyle().Bold(true).Render(wrapText(issue.Title, width)) + "\n\n")
	writeTicketField(&s, "State", issue.State.Name, width)
	writeTicketField(&s, "Priority", linearPriority(issue.Priority), width)
	writeTicketField(&s, "Team", issue.Team.Name, width)
	if issue.Project != nil {
		writeTicketField(&s, "Project", issue.Project.Name, width)
	}
	if len(issue.Labels) > 0 {
		labels := make([]string, 0, len(issue.Labels))
		for _, label := range issue.Labels {
			labels = append(labels, label.Name)
		}
		writeTicketField(&s, "Labels", strings.Join(labels, ", "), width)
	}
	if issue.Description != "" {
		s.WriteString("\n" + dimStyle.Render("Description") + "\n")
		s.WriteString(wrapText(issue.Description, width) + "\n")
	}
	if issue.URL != "" {
		s.WriteString("\n" + dimStyle.Render("URL") + "\n")
		s.WriteString(wrapText(issue.URL, width))
	}
	return strings.TrimRight(s.String(), "\n")
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
		Render(fmt.Sprintf("INTEGRATIONS (%d repos)", len(m.cfg.Repos)))

	health := successStyle.Render("✓ Linear") + " " +
		successStyle.Render("✓ GitHub") + " " +
		successStyle.Render("✓ OpenCode")

	uptime := m.lastUpdate.Sub(m.startedAt)
	if uptime < 0 {
		uptime = 0
	}
	deployment := dimStyle.Render(fmt.Sprintf("%s@%s · up %s",
		api.Version, shortRevision(buildinfo.Revision), formatUptime(uptime)))

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		health,
		deployment,
	)

	return barStyle.Render(content)
}

// renderLogsPane renders the main logs pane with both job logs and server logs
func (m Model) renderLogsPane() string {
	logsWidth := m.width - 32

	// Job Logs Section (TOP) - height includes title (1) + subtitle (1) + viewport + borders (2) = viewport + 4
	jobLogsStyle := lipgloss.NewStyle().
		Width(logsWidth).
		Height(m.jobLogsViewport.Height + 4).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(m.getBorderColor(JobLogsPane))

	jobTitle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("213")).
		Render("JOB LOGS")

	var jobSubtitle string
	if target := m.ticketJob(); target != nil {
		jobSubtitle = dimStyle.Render(fmt.Sprintf("(showing logs for %s)", target.LinearIssueID))
	} else {
		jobSubtitle = dimStyle.Render("(no job selected)")
	}

	jobHeader := lipgloss.JoinVertical(
		lipgloss.Left,
		jobTitle,
		jobSubtitle,
	)

	jobContent := lipgloss.JoinVertical(
		lipgloss.Left,
		jobHeader,
		m.jobLogsViewport.View(),
	)

	// Server Logs Section (BOTTOM) - height includes title (1) + subtitle (1) + viewport + borders (2) = viewport + 4
	serverLogsStyle := lipgloss.NewStyle().
		Width(logsWidth).
		Height(m.serverLogsViewport.Height + 4).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(m.getBorderColor(ServerLogsPane))

	serverTitle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("117")).
		Render("LIVE SERVER LOGS")

	serverSubtitle := dimStyle.Render("(daemon orchestration logs)")

	serverHeader := lipgloss.JoinVertical(
		lipgloss.Left,
		serverTitle,
		serverSubtitle,
	)

	serverContent := lipgloss.JoinVertical(
		lipgloss.Left,
		serverHeader,
		m.serverLogsViewport.View(),
	)

	// Stack both sections vertically: Job Logs above, Server Logs beneath
	return lipgloss.JoinVertical(
		lipgloss.Left,
		jobLogsStyle.Render(jobContent),
		serverLogsStyle.Render(serverContent),
	)
}

// renderServerLogsContent renders the scrollable server logs content
func (m Model) renderServerLogsContent() string {
	var s strings.Builder

	if len(m.serverLogs) == 0 {
		s.WriteString(dimStyle.Render("No server logs available"))
		return s.String()
	}

	for _, log := range m.serverLogs {
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

// renderJobLogsContent renders the scrollable job logs content
func (m Model) renderJobLogsContent() string {
	var s strings.Builder

	if len(m.jobLogs) == 0 {
		s.WriteString(dimStyle.Render("No job logs available"))
		return s.String()
	}

	for _, log := range m.jobLogs {
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
	case TicketInfoPane:
		focusedPaneName = "Ticket Information"
	case ServerLogsPane:
		focusedPaneName = "Server Logs"
	case JobLogsPane:
		focusedPaneName = "Job Logs"
	case IntegrationsPane:
		focusedPaneName = "Integrations"
	}

	// Shortened keybindings to reduce wrapping on narrow terminals
	keybindings := fmt.Sprintf("Focus: %s │ Tab: switch │ ↑↓/jk: nav │ g/G: jump │ r: refresh │ q: quit",
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

		// Get server logs from the global log buffer (live daemon logs)
		// This includes HTTP access logs, orchestration logs, etc.
		var serverLogs []*db.JobLog
		if globalLogBuffer != nil {
			bufferLogs := globalLogBuffer.GetRecent(200)
			for _, entry := range bufferLogs {
				serverLogs = append(serverLogs, &db.JobLog{
					Timestamp: entry.Timestamp,
					Level:     entry.Level,
					Message:   entry.Message,
				})
			}
		}

		// Get job logs for the selected job or current job
		var jobLogs []*db.JobLog
		var targetJob *db.Job

		// Priority: selected job > topmost running job.
		if len(recentJobs) > 0 && m.selectedJobIdx < len(recentJobs) {
			if m.selectedJobIdx >= 0 {
				targetJob = recentJobs[m.selectedJobIdx]
			}
		} else if currentJob != nil {
			targetJob = currentJob
		}
		if targetJob == nil {
			for _, job := range recentJobs {
				if job.State.IsBusy() {
					targetJob = job
					break
				}
			}
		}
		if targetJob == nil {
			targetJob = currentJob
		}

		if targetJob != nil {
			jobLogs, _ = m.database.GetLogs(targetJob.ID, 200)
		}

		return dataRefreshMsg{
			currentJob: currentJob,
			recentJobs: recentJobs,
			serverLogs: serverLogs,
			jobLogs:    jobLogs,
		}
	}
}

// refreshJobLogs refreshes only the job logs when job selection changes
func (m Model) refreshJobLogs() tea.Cmd {
	return func() tea.Msg {
		var jobLogs []*db.JobLog

		// Get logs for the selected job
		if targetJob := m.ticketJob(); targetJob != nil {
			jobLogs, _ = m.database.GetLogs(targetJob.ID, 200)
		}

		return jobLogsRefreshMsg{
			jobLogs: jobLogs,
		}
	}
}

// ticketJob returns the explicitly selected job, or the topmost running job
// when there is no selection.
func (m Model) ticketJob() *db.Job {
	if m.selectedJobIdx >= 0 && m.selectedJobIdx < len(m.recentJobs) {
		return m.recentJobs[m.selectedJobIdx]
	}
	for _, job := range m.recentJobs {
		if job.State.IsBusy() {
			return job
		}
	}
	return m.currentJob
}

func (m *Model) startTicketRefresh() tea.Cmd {
	target := m.ticketJob()
	client := m.issueClient
	if target == nil || client == nil {
		return nil
	}
	jobID, issueID := target.ID, target.LinearIssueID
	m.ticketLoadingFor = jobID
	m.ticketLoadedFor = ""
	m.ticketIssue = nil
	m.ticketError = ""
	return func() tea.Msg {
		issue, err := client.GetIssue(issueID)
		msg := ticketRefreshMsg{jobID: jobID, issue: issue}
		if err != nil {
			msg.err = err.Error()
		}
		return msg
	}
}

// dataRefreshMsg carries refreshed data
type dataRefreshMsg struct {
	currentJob *db.Job
	recentJobs []*db.Job
	serverLogs []*db.JobLog
	jobLogs    []*db.JobLog
}

type ticketRefreshMsg struct {
	jobID string
	issue *linear.Issue
	err   string
}

func writeTicketField(s *strings.Builder, label, value string, width int) {
	if value == "" {
		value = "—"
	}
	s.WriteString(dimStyle.Render(label+": ") + wrapText(value, width-len(label)-2) + "\n")
}

func linearPriority(priority int) string {
	switch priority {
	case 1:
		return "Urgent"
	case 2:
		return "High"
	case 3:
		return "Medium"
	case 4:
		return "Low"
	default:
		return "No priority"
	}
}

func wrapText(text string, width int) string {
	if width < 1 {
		width = 1
	}
	paragraphs := strings.Split(text, "\n")
	for i, paragraph := range paragraphs {
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			paragraphs[i] = ""
			continue
		}
		var expanded []string
		for _, word := range words {
			runes := []rune(word)
			for len(runes) > width {
				expanded = append(expanded, string(runes[:width]))
				runes = runes[width:]
			}
			if len(runes) > 0 {
				expanded = append(expanded, string(runes))
			}
		}
		var lines []string
		line := expanded[0]
		for _, word := range expanded[1:] {
			if len([]rune(line))+1+len([]rune(word)) <= width {
				line += " " + word
			} else {
				lines = append(lines, line)
				line = word
			}
		}
		paragraphs[i] = strings.Join(append(lines, line), "\n")
	}
	return strings.Join(paragraphs, "\n")
}

// jobLogsRefreshMsg carries refreshed job logs only
type jobLogsRefreshMsg struct {
	jobLogs []*db.JobLog
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

func formatUptime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

func shortRevision(revision string) string {
	dirty := strings.HasSuffix(revision, "-dirty")
	revision = strings.TrimSuffix(revision, "-dirty")
	if len(revision) > 7 {
		revision = revision[:7]
	}
	if dirty {
		revision += "*"
	}
	return revision
}

// calculateRenderedHeight calculates how many lines a text string will occupy
// when rendered with lipgloss at a given width, accounting for padding.
// This is critical for proper TUI height calculations when text wraps.
func calculateRenderedHeight(text string, width int, horizontalPadding int) int {
	if width <= 0 {
		return 1
	}

	// Account for padding on both sides
	effectiveWidth := width - (2 * horizontalPadding)
	if effectiveWidth <= 0 {
		effectiveWidth = 1
	}

	// Calculate how many lines the text will occupy
	textLen := len(text)
	if textLen == 0 {
		return 1
	}

	lines := (textLen + effectiveWidth - 1) / effectiveWidth // Ceiling division
	if lines < 1 {
		lines = 1
	}

	return lines
}

// getHeaderHeight estimates the height of the header including wrapping.
// Uses worst-case text length to ensure we never under-allocate.
func (m Model) getHeaderHeight() int {
	if m.width <= 0 {
		return 2 // Minimum with top padding
	}

	// Worst-case header text (with a running job):
	// "devboxd v1.0.0 │ BUSY │ Job: LINEAR-123456789012 (running) │ 999h99m"
	// This is approximately 75 characters in the worst case
	worstCaseHeaderLen := 75

	// Header has horizontal padding of 1 on each side, plus 1 line of top padding
	textHeight := calculateRenderedHeight(strings.Repeat("X", worstCaseHeaderLen), m.width, 1)
	return textHeight + 1 // Add 1 for top padding line
}

// getFooterHeight estimates the height of the footer including wrapping.
// Uses worst-case text length (longest pane name) to ensure we never under-allocate.
func (m Model) getFooterHeight() int {
	if m.width <= 0 {
		return 1
	}

	// Worst-case footer text (with "Integrations" as the focused pane):
	// "Focus: Integrations │ Tab: switch │ ↑↓/jk: nav │ g/G: jump │ r: refresh │ q: quit"
	// This is approximately 85 characters
	worstCaseFooterLen := 85

	// Footer has padding of 1 on each side
	return calculateRenderedHeight(strings.Repeat("X", worstCaseFooterLen), m.width, 1)
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
	return RunContext(context.Background(), cfg, database)
}

// RunContext restores the terminal before a graceful daemon restart.
func RunContext(ctx context.Context, cfg *config.Config, database *db.DB) error {
	return RunContextWithStartedAt(ctx, cfg, database, time.Now())
}

// RunContextWithStartedAt runs the TUI with the daemon instance start time.
func RunContextWithStartedAt(ctx context.Context, cfg *config.Config, database *db.DB, startedAt time.Time) error {
	m := NewModelWithStartedAt(cfg, database, startedAt)
	p := tea.NewProgram(m, tea.WithAltScreen())
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			p.Quit()
		case <-done:
		}
	}()
	_, err := p.Run()
	return err
}

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/nicoalimin/devbox/internal/db"
)

func TestDataRefreshPreservesScrolledLogPositions(t *testing.T) {
	m := Model{
		ready:              true,
		serverLogsViewport: viewport.New(80, 3),
		jobLogsViewport:    viewport.New(80, 3),
		jobsViewport:       viewport.New(20, 3),
		errorsViewport:     viewport.New(20, 3),
	}
	m.serverLogsViewport.SetContent(strings.Repeat("old server line\n", 10))
	m.jobLogsViewport.SetContent(strings.Repeat("old job line\n", 10))
	m.serverLogsViewport.GotoBottom()
	m.jobLogsViewport.GotoBottom()
	m.serverLogsViewport.LineUp(2)
	m.jobLogsViewport.LineUp(3)
	serverOffset := m.serverLogsViewport.YOffset
	jobOffset := m.jobLogsViewport.YOffset

	updatedModel, _ := m.Update(dataRefreshMsg{
		serverLogs: testLogs(12, "server"),
		jobLogs:    testLogs(12, "job"),
	})
	updated := updatedModel.(Model)

	if updated.serverLogsViewport.YOffset != serverOffset {
		t.Fatalf("server log offset = %d, want preserved offset %d", updated.serverLogsViewport.YOffset, serverOffset)
	}
	if updated.jobLogsViewport.YOffset != jobOffset {
		t.Fatalf("job log offset = %d, want preserved offset %d", updated.jobLogsViewport.YOffset, jobOffset)
	}
}

func TestDataRefreshFollowsLogsWhenAlreadyAtBottom(t *testing.T) {
	m := Model{
		ready:              true,
		serverLogsViewport: viewport.New(80, 3),
		jobLogsViewport:    viewport.New(80, 3),
		jobsViewport:       viewport.New(20, 3),
		errorsViewport:     viewport.New(20, 3),
	}
	m.serverLogsViewport.SetContent(strings.Repeat("old server line\n", 10))
	m.jobLogsViewport.SetContent(strings.Repeat("old job line\n", 10))
	m.serverLogsViewport.GotoBottom()
	m.jobLogsViewport.GotoBottom()

	updatedModel, _ := m.Update(dataRefreshMsg{
		serverLogs: testLogs(12, "server"),
		jobLogs:    testLogs(12, "job"),
	})
	updated := updatedModel.(Model)

	if !updated.serverLogsViewport.AtBottom() {
		t.Fatal("server logs stopped following while already at bottom")
	}
	if !updated.jobLogsViewport.AtBottom() {
		t.Fatal("job logs stopped following while already at bottom")
	}
}

func testLogs(count int, prefix string) []*db.JobLog {
	logs := make([]*db.JobLog, count)
	for i := range logs {
		logs[i] = &db.JobLog{
			Timestamp: time.Unix(int64(i), 0),
			Level:     "info",
			Message:   prefix,
		}
	}
	return logs
}

func TestCalculateRenderedHeight(t *testing.T) {
	tests := []struct {
		name              string
		text              string
		width             int
		horizontalPadding int
		expectedHeight    int
	}{
		{
			name:              "Short text fits on one line",
			text:              "Hello",
			width:             80,
			horizontalPadding: 1,
			expectedHeight:    1,
		},
		{
			name:              "Text exactly fits width",
			text:              strings.Repeat("X", 78), // 80 - 2 padding
			width:             80,
			horizontalPadding: 1,
			expectedHeight:    1,
		},
		{
			name:              "Text wraps to two lines",
			text:              strings.Repeat("X", 79), // 80 - 2 padding + 1
			width:             80,
			horizontalPadding: 1,
			expectedHeight:    2,
		},
		{
			name:              "Footer at 80 cols wraps to 2 lines",
			text:              "Focus: Integrations │ Tab: switch │ ↑↓/jk: nav │ g/G: jump │ r: refresh │ q: quit",
			width:             80,
			horizontalPadding: 1,
			expectedHeight:    2, // 85 chars / 78 effective width = 2 lines
		},
		{
			name:              "Footer at 120 cols fits on 1 line",
			text:              "Focus: Integrations │ Tab: switch │ ↑↓/jk: nav │ g/G: jump │ r: refresh │ q: quit",
			width:             120,
			horizontalPadding: 1,
			expectedHeight:    1, // 85 chars / 118 effective width = 1 line
		},
		{
			name:              "Empty text",
			text:              "",
			width:             80,
			horizontalPadding: 1,
			expectedHeight:    1,
		},
		{
			name:              "Zero width",
			text:              "Hello",
			width:             0,
			horizontalPadding: 1,
			expectedHeight:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateRenderedHeight(tt.text, tt.width, tt.horizontalPadding)
			if got != tt.expectedHeight {
				t.Errorf("calculateRenderedHeight() = %v, want %v (text len=%d, width=%d, padding=%d)",
					got, tt.expectedHeight, len(tt.text), tt.width, tt.horizontalPadding)
			}
		})
	}
}

func TestGetHeaderHeight(t *testing.T) {
	tests := []struct {
		name           string
		width          int
		expectedHeight int
	}{
		{
			name:           "Wide terminal (160 cols) - header fits on 1 line",
			width:          160,
			expectedHeight: 2, // 1 line text + 1 line top padding
		},
		{
			name:           "Medium terminal (120 cols) - header fits on 1 line",
			width:          120,
			expectedHeight: 2, // 1 line text + 1 line top padding
		},
		{
			name:           "Narrow terminal (80 cols) - header may wrap",
			width:          80,
			expectedHeight: 2, // 1 line text (75 chars fit at 80 cols) + 1 line top padding
		},
		{
			name:           "Very narrow terminal (60 cols) - header wraps",
			width:          60,
			expectedHeight: 3, // 2 lines text (75 chars / 58 effective width) + 1 line top padding
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{width: tt.width}
			got := m.getHeaderHeight()
			if got != tt.expectedHeight {
				t.Errorf("getHeaderHeight() = %v, want %v (width=%d)", got, tt.expectedHeight, tt.width)
			}
		})
	}
}

func TestGetFooterHeight(t *testing.T) {
	tests := []struct {
		name           string
		width          int
		expectedHeight int
	}{
		{
			name:           "Wide terminal (160 cols) - footer fits on 1 line",
			width:          160,
			expectedHeight: 1,
		},
		{
			name:           "Medium terminal (120 cols) - footer fits on 1 line",
			width:          120,
			expectedHeight: 1,
		},
		{
			name:           "Narrow terminal (80 cols) - footer wraps to 2 lines",
			width:          80,
			expectedHeight: 2, // 85 chars / 78 effective width = 2 lines
		},
		{
			name:           "Very narrow terminal (60 cols) - footer wraps to 2 lines",
			width:          60,
			expectedHeight: 2, // 85 chars / 58 effective width = 2 lines
		},
		{
			name:           "Ultra narrow terminal (40 cols) - footer wraps to 3 lines",
			width:          40,
			expectedHeight: 3, // 85 chars / 38 effective width = 3 lines
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{width: tt.width}
			got := m.getFooterHeight()
			if got != tt.expectedHeight {
				t.Errorf("getFooterHeight() = %v, want %v (width=%d)", got, tt.expectedHeight, tt.width)
			}
		})
	}
}

// TestAvailableContentHeight verifies that the content height calculation
// correctly accounts for header and footer wrapping at different terminal widths.
func TestAvailableContentHeight(t *testing.T) {
	tests := []struct {
		name                     string
		terminalHeight           int
		terminalWidth            int
		expectedAvailableContent int
		description              string
	}{
		{
			name:                     "80x24 terminal (standard)",
			terminalHeight:           24,
			terminalWidth:            80,
			expectedAvailableContent: 18, // 24 - 2 (header with top padding) - 2 (footer wraps) - 2 (newlines) = 18
			description:              "Footer wraps to 2 lines at 80 cols",
		},
		{
			name:                     "120x30 terminal",
			terminalHeight:           30,
			terminalWidth:            120,
			expectedAvailableContent: 25, // 30 - 2 (header with top padding) - 1 (footer) - 2 (newlines) = 25
			description:              "Footer fits on 1 line at 120 cols",
		},
		{
			name:                     "160x40 terminal (wide)",
			terminalHeight:           40,
			terminalWidth:            160,
			expectedAvailableContent: 35, // 40 - 2 (header with top padding) - 1 (footer) - 2 (newlines) = 35
			description:              "Footer fits on 1 line at 160 cols",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{
				width:  tt.terminalWidth,
				height: tt.terminalHeight,
			}

			headerHeight := m.getHeaderHeight()
			footerHeight := m.getFooterHeight()
			availableContent := tt.terminalHeight - headerHeight - footerHeight - 2

			if availableContent != tt.expectedAvailableContent {
				t.Errorf("%s: availableContentHeight = %v, want %v (header=%d, footer=%d, total=%d)",
					tt.description, availableContent, tt.expectedAvailableContent,
					headerHeight, footerHeight, tt.terminalHeight)
			}
		})
	}
}

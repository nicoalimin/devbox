package tui

import (
	"strings"
	"testing"
)

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
			expectedHeight: 1,
		},
		{
			name:           "Medium terminal (120 cols) - header fits on 1 line",
			width:          120,
			expectedHeight: 1,
		},
		{
			name:           "Narrow terminal (80 cols) - header may wrap",
			width:          80,
			expectedHeight: 1, // 75 chars should still fit at 80 cols with padding
		},
		{
			name:           "Very narrow terminal (60 cols) - header wraps",
			width:          60,
			expectedHeight: 2, // 75 chars / 58 effective width = 2 lines
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
		name                        string
		terminalHeight              int
		terminalWidth               int
		expectedAvailableContent    int
		description                 string
	}{
		{
			name:                     "80x24 terminal (standard)",
			terminalHeight:           24,
			terminalWidth:            80,
			expectedAvailableContent: 19, // 24 - 1 (header) - 2 (footer wraps) - 2 (newlines) = 19
			description:              "Footer wraps to 2 lines at 80 cols",
		},
		{
			name:                     "120x30 terminal",
			terminalHeight:           30,
			terminalWidth:            120,
			expectedAvailableContent: 26, // 30 - 1 (header) - 1 (footer) - 2 (newlines) = 26
			description:              "Footer fits on 1 line at 120 cols",
		},
		{
			name:                     "160x40 terminal (wide)",
			terminalHeight:           40,
			terminalWidth:            160,
			expectedAvailableContent: 36, // 40 - 1 (header) - 1 (footer) - 2 (newlines) = 36
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

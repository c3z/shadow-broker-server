package main

import (
	"crypto/ed25519"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// wsEventMsg carries an incoming broker message into the TUI event loop.
type wsEventMsg struct {
	id  int64
	raw string
}

// wsStateMsg carries a WebSocket connection state change into the TUI.
type wsStateMsg struct{ connected bool }

// sendResultMsg reports the outcome of an async publish.
type sendResultMsg struct{ err error }

// tuiStyles holds the lipgloss styles used by the TUI.
var tuiStatusStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#FFFFFF")).
	Background(lipgloss.Color("#5F5FD7")).
	Width(100)

// tuiModel is the bubbletea model for the interactive queue view.
type tuiModel struct {
	config    Config
	queue     string
	priv      ed25519.PrivateKey // nil => send plain messages
	from      string
	pubKey    ed25519.PublicKey // nil => no signature verification
	client    *ShadowClient
	events    chan tea.Msg
	viewport  viewport.Model
	input     textinput.Model
	lines     []string
	count     int
	connected bool
	ready     bool
	width     int
	height    int
	notice    string // transient status line note (e.g. last send error)
}

// newTUIModel builds the model, wires the WS handlers and starts the
// subscription in the background.
func newTUIModel(config Config, queue string, priv ed25519.PrivateKey, from string, pubKey ed25519.PublicKey) *tuiModel {
	input := textinput.New()
	input.Placeholder = "type a message, /queue <name> to switch, Esc to quit"
	input.Focus()

	m := &tuiModel{
		config: config,
		queue:  queue,
		priv:   priv,
		from:   from,
		pubKey: pubKey,
		events: make(chan tea.Msg, 64),
		input:  input,
	}
	m.startClient(queue)
	return m
}

// startClient creates a fresh client subscription for the given queue.
// A new ShadowClient is used each time because wsStopChan is single-use.
func (m *tuiModel) startClient(queue string) {
	m.client = NewShadowClient(m.config)
	m.client.onMessage = func(id int64, raw string) {
		m.events <- wsEventMsg{id: id, raw: raw}
	}
	m.client.onState = func(connected bool) {
		m.events <- wsStateMsg{connected: connected}
	}
	m.client.StartSubscribe(queue, m.pubKey)
	m.queue = queue
	m.connected = false
}

// stopClient shuts down the current subscription, if any.
func (m *tuiModel) stopClient() {
	if m.client != nil {
		m.client.StopSubscribe()
		m.client = nil
	}
}

// waitEvent blocks on the events channel; it is re-issued after every event
// so the TUI keeps receiving WS events.
func waitEvent(events chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return <-events
	}
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(waitEvent(m.events), textinput.Blink)
}

// refreshViewport re-renders the message list and keeps it scrolled down.
func (m *tuiModel) refreshViewport() {
	m.viewport.SetContent(strings.Join(m.lines, "\n"))
	m.viewport.GotoBottom()
}

// appendLine adds a rendered line to the message list.
func (m *tuiModel) appendLine(line string) {
	m.lines = append(m.lines, line)
	m.refreshViewport()
}

// sendCmd publishes text to the current queue asynchronously, signing it
// first when a private key is configured.
func (m *tuiModel) sendCmd(text string) tea.Cmd {
	queue := m.queue
	message := text
	if m.priv != nil {
		signed, err := signEnvelope(message, m.from, m.priv)
		if err != nil {
			return func() tea.Msg { return sendResultMsg{err: err} }
		}
		message = signed
	}
	client := m.client
	return func() tea.Msg {
		if client == nil {
			return sendResultMsg{err: fmt.Errorf("not connected")}
		}
		_, err := client.Publish(queue, message)
		return sendResultMsg{err: err}
	}
}

// handleInput processes one submitted input line.
func (m *tuiModel) handleInput(text string) tea.Cmd {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if strings.HasPrefix(text, "/queue") {
		name := strings.TrimSpace(strings.TrimPrefix(text, "/queue"))
		if name == "" {
			m.notice = "usage: /queue <name>"
			return nil
		}
		m.stopClient()
		m.startClient(name)
		m.appendLine(fmt.Sprintf("--- switched to queue %q ---", name))
		m.notice = ""
		return nil
	}
	m.notice = ""
	return m.sendCmd(text)
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		bodyHeight := msg.Height - 2 // input line + status bar
		if bodyHeight < 1 {
			bodyHeight = 1
		}
		if !m.ready {
			m.viewport = viewport.New(msg.Width, bodyHeight)
			m.ready = true
			m.refreshViewport()
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = bodyHeight
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEscape:
			m.stopClient()
			return m, tea.Quit
		case tea.KeyEnter:
			cmd := m.handleInput(m.input.Value())
			m.input.SetValue("")
			return m, cmd
		}

	case wsEventMsg:
		m.count++
		m.appendLine(formatReceived(msg.raw, m.pubKey))
		return m, waitEvent(m.events)

	case wsStateMsg:
		m.connected = msg.connected
		return m, waitEvent(m.events)

	case sendResultMsg:
		if msg.err != nil {
			m.notice = "send failed: " + msg.err.Error()
		} else {
			m.notice = ""
		}
		return m, nil
	}

	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *tuiModel) View() string {
	if !m.ready {
		return "Connecting..."
	}
	state := "connected"
	if !m.connected {
		state = "reconnecting"
	}
	status := fmt.Sprintf(" queue: %s | %s | messages: %d", m.queue, state, m.count)
	if m.notice != "" {
		status += " | " + m.notice
	}
	statusBar := tuiStatusStyle.Width(m.width).Render(status)
	return fmt.Sprintf("%s\n%s\n%s", m.viewport.View(), m.input.View(), statusBar)
}

// runTUI starts the interactive terminal UI. It returns an error (instead of
// panicking) when no TTY is available.
func runTUI(config Config, queue string, priv ed25519.PrivateKey, from string, pubKey ed25519.PublicKey) error {
	// Keep reconnect/heartbeat logs out of the alternate screen; restore
	// stderr logging on exit so the caller can report a startup error.
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	model := newTUIModel(config, queue, priv, from, pubKey)
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	model.stopClient()
	return err
}

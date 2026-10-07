//go:build windows

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/widget"
)

const (
	appID          = "uk.laura.notification-inbox"
	windowTitle    = "Notification Inbox"
	defaultListen  = "127.0.0.1:55583"
	maxRequestBody = 64 * 1024
)

type IncomingNotification struct {
	Priority int     `json:"priority"`
	Colour   string  `json:"colour"`
	Color    string  `json:"color"`
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	Icon     *string `json:"icon"`
	Link     *string `json:"link"`
}

type NotificationItem struct {
	ID             uint64     `json:"id"`
	Priority       int        `json:"priority"`
	Colour         string     `json:"colour"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	Icon           string     `json:"icon,omitempty"`
	Link           string     `json:"link,omitempty"`
	ReceivedAt     time.Time  `json:"received_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
}

type Settings struct {
	TaskbarFlash         bool   `json:"taskbar_flash"`
	WindowsNotifications bool   `json:"windows_notifications"`
	SoundsEnabled        bool   `json:"sounds_enabled"`
	NormalSoundPath      string `json:"normal_sound_path"`
	CriticalSoundPath    string `json:"critical_sound_path"`
	Priority4Seconds     int    `json:"priority_4_seconds"`
	RepeatGapMS          int    `json:"repeat_gap_ms"`
	FlashCount           int    `json:"flash_count"`
	NewestFirst          bool   `json:"newest_first"`
	PersistPending       bool   `json:"persist_pending"`
	ShowHelp             bool   `json:"show_help"`
	AuthToken            string `json:"auth_token"`
}

type savedInbox struct {
	NextID uint64             `json:"next_id"`
	Items  []NotificationItem `json:"items"`
}

type State struct {
	mu        sync.RWMutex
	persistMu sync.Mutex

	items        []NotificationItem
	nextID       uint64
	settings     Settings
	settingsPath string
	inboxPath    string
}

func defaultSettings() Settings {
	return Settings{
		TaskbarFlash:         true,
		WindowsNotifications: true,
		SoundsEnabled:        true,
		Priority4Seconds:     30,
		RepeatGapMS:          600,
		FlashCount:           3,
		NewestFirst:          true,
		PersistPending:       true,
		ShowHelp:             true,
	}
}

func newState(settingsPath, inboxPath string) *State {
	return &State{
		settings:     defaultSettings(),
		settingsPath: settingsPath,
		inboxPath:    inboxPath,
	}
}

func (s *State) Load() error {
	var errs []error

	if data, err := os.ReadFile(s.settingsPath); err == nil {
		settings := defaultSettings()
		if err := json.Unmarshal(data, &settings); err != nil {
			errs = append(errs, fmt.Errorf("settings: %w", err))
		} else {
			normaliseSettings(&settings)
			s.mu.Lock()
			s.settings = settings
			s.mu.Unlock()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("settings: %w", err))
	}

	settings := s.Settings()
	if settings.PersistPending {
		if data, err := os.ReadFile(s.inboxPath); err == nil {
			var inbox savedInbox
			if err := json.Unmarshal(data, &inbox); err != nil {
				errs = append(errs, fmt.Errorf("pending inbox: %w", err))
			} else {
				s.mu.Lock()
				s.items = inbox.Items
				s.nextID = inbox.NextID
				for _, item := range inbox.Items {
					if item.ID > s.nextID {
						s.nextID = item.ID
					}
				}
				s.mu.Unlock()
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("pending inbox: %w", err))
		}
	}

	return errors.Join(errs...)
}

func (s *State) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

func (s *State) SetSettings(settings Settings) error {
	normaliseSettings(&settings)

	s.mu.Lock()
	s.settings = settings
	s.mu.Unlock()

	if err := writeJSONFile(s.settingsPath, settings); err != nil {
		return err
	}

	if settings.PersistPending {
		return s.saveInbox()
	}
	if err := os.Remove(s.inboxPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func normaliseSettings(settings *Settings) {
	settings.NormalSoundPath = strings.TrimSpace(settings.NormalSoundPath)
	settings.CriticalSoundPath = strings.TrimSpace(settings.CriticalSoundPath)
	settings.AuthToken = strings.TrimSpace(settings.AuthToken)
	settings.Priority4Seconds = clampInt(settings.Priority4Seconds, 1, 600)
	settings.RepeatGapMS = clampInt(settings.RepeatGapMS, 0, 10000)
	settings.FlashCount = clampInt(settings.FlashCount, 1, 20)
}

func (s *State) Add(in IncomingNotification) (NotificationItem, error) {
	item, err := validateIncoming(in)
	if err != nil {
		return NotificationItem{}, err
	}

	s.mu.Lock()
	s.nextID++
	item.ID = s.nextID
	item.ReceivedAt = time.Now()
	s.items = append(s.items, item)
	persist := s.settings.PersistPending
	s.mu.Unlock()

	if persist {
		_ = s.saveInbox()
	}
	return item, nil
}

func (s *State) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

func (s *State) At(index int) (NotificationItem, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if index < 0 || index >= len(s.items) {
		return NotificationItem{}, false
	}
	if s.settings.NewestFirst {
		index = len(s.items) - 1 - index
	}
	return s.items[index], true
}

func (s *State) Acknowledge(id uint64) bool {
	s.mu.Lock()
	found := false
	for i := range s.items {
		if s.items[i].ID != id {
			continue
		}
		if s.items[i].AcknowledgedAt != nil {
			break
		}
		now := time.Now()
		s.items[i].AcknowledgedAt = &now
		found = true
		break
	}
	persist := s.settings.PersistPending
	s.mu.Unlock()

	if found && persist {
		_ = s.saveInbox()
	}
	return found
}

func (s *State) Dismiss(id uint64) bool {
	s.mu.Lock()
	found := false
	for i := range s.items {
		if s.items[i].ID == id {
			copy(s.items[i:], s.items[i+1:])
			s.items = s.items[:len(s.items)-1]
			found = true
			break
		}
	}
	persist := s.settings.PersistPending
	s.mu.Unlock()

	if found && persist {
		_ = s.saveInbox()
	}
	return found
}

func (s *State) DismissAll() []uint64 {
	s.mu.Lock()
	ids := make([]uint64, 0, len(s.items))
	for _, item := range s.items {
		ids = append(ids, item.ID)
	}
	s.items = nil
	persist := s.settings.PersistPending
	s.mu.Unlock()

	if persist {
		_ = s.saveInbox()
	}
	return ids
}

func (s *State) RefreshOrder(newestFirst bool) error {
	settings := s.Settings()
	settings.NewestFirst = newestFirst
	return s.SetSettings(settings)
}

func (s *State) saveInbox() error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	s.mu.RLock()
	inbox := savedInbox{
		NextID: s.nextID,
		Items:  append([]NotificationItem(nil), s.items...),
	}
	s.mu.RUnlock()
	return writeJSONFile(s.inboxPath, inbox)
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func configPaths() (string, string) {
	configDir, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(configDir) == "" {
		return "notification-inbox-settings.json", "notification-inbox-pending.json"
	}
	base := filepath.Join(configDir, "notification-inbox")
	return filepath.Join(base, "settings.json"), filepath.Join(base, "pending.json")
}

func validateIncoming(in IncomingNotification) (NotificationItem, error) {
	if in.Priority < 1 || in.Priority > 5 {
		return NotificationItem{}, errors.New("priority must be an integer from 1 to 5")
	}

	title := strings.TrimSpace(in.Title)
	body := strings.TrimSpace(in.Body)
	if title == "" {
		return NotificationItem{}, errors.New("missing title")
	}
	if utf8.RuneCountInString(title) > 200 {
		return NotificationItem{}, errors.New("title is too long (maximum 200 characters)")
	}
	if utf8.RuneCountInString(body) > 8000 {
		return NotificationItem{}, errors.New("body is too long (maximum 8000 characters)")
	}

	colourText := firstNonEmpty(in.Colour, in.Color)
	if colourText == "" {
		colourText = defaultColourForPriority(in.Priority)
	}
	colourText, err := normaliseColour(colourText)
	if err != nil {
		return NotificationItem{}, err
	}

	icon := ""
	if in.Icon != nil {
		icon = strings.TrimSpace(*in.Icon)
		if utf8.RuneCountInString(icon) > 12 {
			return NotificationItem{}, errors.New("icon is too long (maximum 12 characters)")
		}
	}

	link := ""
	if in.Link != nil {
		link = strings.TrimSpace(*in.Link)
		if link != "" {
			parsed, err := url.ParseRequestURI(link)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				return NotificationItem{}, errors.New("link must be an absolute http:// or https:// URL")
			}
		}
	}

	return NotificationItem{
		Priority: in.Priority,
		Colour:   colourText,
		Title:    title,
		Body:     body,
		Icon:     icon,
		Link:     link,
	}, nil
}

func defaultColourForPriority(priority int) string {
	switch priority {
	case 1:
		return "#A0A0A0"
	case 2:
		return "#6EA8FE"
	case 3:
		return "#38BDF8"
	case 4:
		return "#F59E0B"
	case 5:
		return "#FB7185"
	default:
		return "#FFFFFF"
	}
}

func normaliseColour(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("colour cannot be empty")
	}
	if !strings.HasPrefix(value, "#") {
		value = "#" + value
	}
	if len(value) != 7 {
		return "", errors.New("colour must be #RRGGBB")
	}
	if _, err := strconv.ParseUint(value[1:], 16, 24); err != nil {
		return "", errors.New("colour must be #RRGGBB")
	}
	return strings.ToUpper(value), nil
}

func colourFromHex(value string) color.Color {
	normalised, err := normaliseColour(value)
	if err != nil {
		return color.White
	}
	r, _ := strconv.ParseUint(normalised[1:3], 16, 8)
	g, _ := strconv.ParseUint(normalised[3:5], 16, 8)
	b, _ := strconv.ParseUint(normalised[5:7], 16, 8)
	return color.NRGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 0xff}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func clampInt(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

// AlertManager owns every sound loop so acknowledging one notification can stop
// exactly that notification without affecting any other pending item.
type AlertManager struct {
	mu sync.Mutex

	cancels map[uint64]context.CancelFunc
	state   *State
	app     fyne.App
}

func newAlertManager(state *State, a fyne.App) *AlertManager {
	return &AlertManager{
		cancels: make(map[uint64]context.CancelFunc),
		state:   state,
		app:     a,
	}
}

func (m *AlertManager) Start(item NotificationItem) {
	settings := m.state.Settings()

	if item.Priority >= 2 && settings.TaskbarFlash {
		go flashTaskbar(windowTitle, uint32(settings.FlashCount))
	}

	if item.Priority >= 3 && settings.WindowsNotifications {
		content := item.Body
		if item.Icon != "" {
			content = strings.TrimSpace(item.Icon + " " + content)
		}
		m.app.SendNotification(fyne.NewNotification(item.Title, content))
	}

	if item.Priority < 3 || !settings.SoundsEnabled {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if old, ok := m.cancels[item.ID]; ok {
		old()
	}
	m.cancels[item.ID] = cancel
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.cancels, item.ID)
			m.mu.Unlock()
		}()

		switch item.Priority {
		case 3:
			_ = playSoundOnce(ctx, item.ID, 0, settings.NormalSoundPath, false)
		case 4:
			timedCtx, timedCancel := context.WithTimeout(ctx, time.Duration(settings.Priority4Seconds)*time.Second)
			defer timedCancel()
			m.repeatSound(timedCtx, item.ID, settings.NormalSoundPath, false, settings.RepeatGapMS)
		case 5:
			m.repeatSound(ctx, item.ID, settings.CriticalSoundPath, true, settings.RepeatGapMS)
		}
	}()
}

func (m *AlertManager) repeatSound(ctx context.Context, id uint64, path string, critical bool, gapMS int) {
	sequence := uint64(0)
	for {
		if ctx.Err() != nil {
			return
		}
		sequence++
		_ = playSoundOnce(ctx, id, sequence, path, critical)
		if ctx.Err() != nil {
			return
		}
		if !sleepContext(ctx, time.Duration(gapMS)*time.Millisecond) {
			return
		}
	}
}

func (m *AlertManager) Stop(id uint64) {
	m.mu.Lock()
	cancel := m.cancels[id]
	delete(m.cancels, id)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *AlertManager) StopAll(ids []uint64) {
	for _, id := range ids {
		m.Stop(id)
	}
}

func (m *AlertManager) StopEverything() {
	m.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(m.cancels))
	for id, cancel := range m.cancels {
		cancels = append(cancels, cancel)
		delete(m.cancels, id)
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

var (
	winmm         = syscall.NewLazyDLL("winmm.dll")
	mciSendString = winmm.NewProc("mciSendStringW")

	user32        = syscall.NewLazyDLL("user32.dll")
	findWindowW   = user32.NewProc("FindWindowW")
	flashWindowEx = user32.NewProc("FlashWindowEx")
	messageBeep   = user32.NewProc("MessageBeep")
)

type flashWInfo struct {
	CbSize    uint32
	Hwnd      uintptr
	DwFlags   uint32
	UCount    uint32
	DwTimeout uint32
}

const flashWTray = 0x00000002

func flashTaskbar(title string, count uint32) {
	titlePtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	hwnd, _, _ := findWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))
	if hwnd == 0 {
		return
	}
	info := flashWInfo{
		CbSize:  uint32(unsafe.Sizeof(flashWInfo{})),
		Hwnd:    hwnd,
		DwFlags: flashWTray,
		UCount:  count,
	}
	_, _, _ = flashWindowEx.Call(uintptr(unsafe.Pointer(&info)))
}

func mciCommand(command string) error {
	commandPtr, err := syscall.UTF16PtrFromString(command)
	if err != nil {
		return err
	}
	result, _, _ := mciSendString.Call(
		uintptr(unsafe.Pointer(commandPtr)),
		0,
		0,
		0,
	)
	if result != 0 {
		return fmt.Errorf("mciSendStringW failed with code %d", result)
	}
	return nil
}

func systemBeep(critical bool) {
	kind := uintptr(0x00000040) // MB_ICONASTERISK
	if critical {
		kind = 0x00000010 // MB_ICONHAND
	}
	_, _, _ = messageBeep.Call(kind)
}

func playSoundOnce(ctx context.Context, id, sequence uint64, path string, critical bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	path = strings.TrimSpace(path)
	if path == "" {
		systemBeep(critical)
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		systemBeep(critical)
		return err
	}

	alias := fmt.Sprintf("notifyAlert%d_%d", id, sequence)
	_ = mciCommand("close " + alias)
	if err := mciCommand(fmt.Sprintf(`open "%s" alias %s`, path, alias)); err != nil {
		systemBeep(critical)
		return err
	}
	defer func() { _ = mciCommand("close " + alias) }()

	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = mciCommand("stop " + alias)
		case <-stopped:
		}
	}()

	err := mciCommand("play " + alias + " from 0 wait")
	close(stopped)
	return err
}

type notificationRow struct {
	widget.BaseWidget

	background   *canvas.Rectangle
	icon         *widget.Label
	priority     *canvas.Text
	title        *canvas.Text
	timeLabel    *widget.Label
	body         *widget.Label
	openButton   *widget.Button
	actionButton *widget.Button
	boundID      uint64
}

func newNotificationRow() *notificationRow {
	r := &notificationRow{
		background:   canvas.NewRectangle(color.NRGBA{A: 0}),
		icon:         widget.NewLabel(""),
		priority:     canvas.NewText("P1", color.White),
		title:        canvas.NewText("Notification", color.White),
		timeLabel:    widget.NewLabel("00:00:00"),
		body:         widget.NewLabel("Body"),
		openButton:   widget.NewButton("Open", nil),
		actionButton: widget.NewButton("Dismiss", nil),
	}
	r.priority.TextStyle = fyne.TextStyle{Bold: true}
	r.title.TextStyle = fyne.TextStyle{Bold: true}
	r.body.Wrapping = fyne.TextWrapWord
	r.timeLabel.Importance = widget.LowImportance
	r.openButton.Importance = widget.LowImportance
	r.actionButton.Importance = widget.LowImportance
	r.ExtendBaseWidget(r)
	return r
}

func (r *notificationRow) CreateRenderer() fyne.WidgetRenderer {
	header := container.NewHBox(
		r.icon,
		r.priority,
		r.title,
		layout.NewSpacer(),
		r.timeLabel,
	)
	actions := container.NewHBox(layout.NewSpacer(), r.openButton, r.actionButton)
	content := container.NewVBox(header, r.body, actions)
	return widget.NewSimpleRenderer(container.NewStack(r.background, content))
}

func notificationBackground(value string) color.Color {
	c := colourFromHex(value)
	switch v := c.(type) {
	case color.NRGBA:
		v.A = 0x24
		return v
	case color.RGBA:
		return color.NRGBA{R: v.R, G: v.G, B: v.B, A: 0x24}
	default:
		r, g, b, _ := c.RGBA()
		return color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: 0x24}
	}
}

func estimatedRowHeight(item NotificationItem) float32 {
	bodyRunes := utf8.RuneCountInString(item.Body)
	lines := 1
	if bodyRunes > 0 {
		lines = (bodyRunes + 89) / 90
	}
	newlines := strings.Count(item.Body, "\n")
	if newlines+1 > lines {
		lines = newlines + 1
	}
	lines = clampInt(lines, 1, 8)

	// Header + body + action button need more room than the default List row.
	// Keeping this explicit prevents the action button from spilling into the
	// following notification when Fyne lays out a compact row.
	return float32(112 + (lines-1)*20)
}

func main() {
	listenAddr := flag.String("listen", defaultListen, "HTTP listen address")
	flag.Parse()

	settingsPath, inboxPath := configPaths()
	state := newState(settingsPath, inboxPath)
	loadErr := state.Load()

	a := app.NewWithID(appID)
	w := a.NewWindow(windowTitle)
	w.Resize(fyne.NewSize(820, 720))

	alerts := newAlertManager(state, a)

	statusLabel := widget.NewLabel("Starting HTTP receiver…")
	statusLabel.Wrapping = fyne.TextWrapWord
	statusLabel.Importance = widget.LowImportance

	pendingLabel := widget.NewLabel("0 pending")
	pendingLabel.TextStyle = fyne.TextStyle{Bold: true}

	var notificationList *widget.List
	refresh := func(scroll bool) {
		count := state.Count()
		if count == 1 {
			pendingLabel.SetText("1 pending")
		} else {
			pendingLabel.SetText(fmt.Sprintf("%d pending", count))
		}
		for i := 0; i < count; i++ {
			if item, ok := state.At(i); ok {
				notificationList.SetItemHeight(i, estimatedRowHeight(item))
			}
		}
		notificationList.Refresh()
		if scroll && count > 0 && !state.Settings().NewestFirst {
			notificationList.ScrollToBottom()
		}
	}

	notificationList = widget.NewList(
		func() int { return state.Count() },
		func() fyne.CanvasObject { return newNotificationRow() },
		func(id widget.ListItemID, object fyne.CanvasObject) {
			row := object.(*notificationRow)
			item, ok := state.At(int(id))
			if !ok {
				row.boundID = 0
				row.background.FillColor = color.NRGBA{A: 0}
				canvas.Refresh(row.background)
				row.icon.SetText("")
				row.priority.Text = ""
				row.title.Text = ""
				row.timeLabel.SetText("")
				row.body.SetText("")
				row.openButton.Hide()
				row.actionButton.Hide()
				row.actionButton.OnTapped = nil
				return
			}

			row.boundID = item.ID
			row.background.FillColor = notificationBackground(item.Colour)
			canvas.Refresh(row.background)

			row.icon.SetText(item.Icon)
			if item.Icon == "" {
				row.icon.Hide()
			} else {
				row.icon.Show()
			}
			row.priority.Text = fmt.Sprintf("P%d", item.Priority)
			row.priority.Color = colourFromHex(item.Colour)
			canvas.Refresh(row.priority)
			row.title.Text = item.Title
			row.title.Color = colourFromHex(item.Colour)
			canvas.Refresh(row.title)
			row.timeLabel.SetText(item.ReceivedAt.Format("15:04:05"))
			row.body.SetText(item.Body)

			itemID := item.ID
			if item.Link == "" {
				row.openButton.Hide()
				row.openButton.OnTapped = nil
			} else {
				link := item.Link
				row.openButton.Show()
				row.openButton.OnTapped = func() {
					u, err := url.Parse(link)
					if err != nil {
						statusLabel.SetText("Could not open link: " + err.Error())
						return
					}
					if err := a.OpenURL(u); err != nil {
						statusLabel.SetText("Could not open link: " + err.Error())
					}
				}
			}
			row.openButton.Refresh()

			setDismissAction := func() {
				row.actionButton.SetText("Dismiss")
				row.actionButton.Enable()
				row.actionButton.OnTapped = func() {
					alerts.Stop(itemID)
					if state.Dismiss(itemID) {
						refresh(false)
					}
				}
			}

			row.actionButton.Show()
			if item.Priority < 4 {
				setDismissAction()
			} else if item.AcknowledgedAt == nil {
				row.actionButton.SetText("Acknowledge")
				row.actionButton.Enable()
				row.actionButton.OnTapped = func() {
					alerts.Stop(itemID)
					if !state.Acknowledge(itemID) {
						return
					}

					// Acknowledge stops the repeating behaviour but deliberately keeps
					// the notification in the inbox. Briefly disable the button so a
					// double-click cannot immediately dismiss it as well.
					row.actionButton.SetText("Acknowledged")
					row.actionButton.Disable()
					row.actionButton.Refresh()

					boundID := itemID
					time.AfterFunc(2*time.Second, func() {
						fyne.Do(func() {
							if row.boundID != boundID {
								return
							}
							setDismissAction()
							row.actionButton.Refresh()
						})
					})
				}
			} else {
				remaining := 2*time.Second - time.Since(*item.AcknowledgedAt)
				if remaining > 0 {
					row.actionButton.SetText("Acknowledged")
					row.actionButton.Disable()
					row.actionButton.OnTapped = nil
					boundID := itemID
					time.AfterFunc(remaining, func() {
						fyne.Do(func() {
							if row.boundID != boundID {
								return
							}
							setDismissAction()
							row.actionButton.Refresh()
						})
					})
				} else {
					setDismissAction()
				}
			}
			row.actionButton.Refresh()
		},
	)

	// List selection would otherwise paint an opaque theme selection colour
	// over the notification's own tint. The rows already contain their own
	// controls, so selection itself has no useful action here.
	notificationList.OnSelected = func(id widget.ListItemID) {
		notificationList.Unselect(id)
	}

	dismissAllButton := widget.NewButton("Dismiss all", func() {
		ids := state.DismissAll()
		alerts.StopAll(ids)
		refresh(false)
	})
	dismissAllButton.Importance = widget.LowImportance

	settingsButton := widget.NewButton("⋯", nil)
	settingsButton.Importance = widget.LowImportance
	settingsButton.OnTapped = func() {
		showSettingsDialog(w, state, statusLabel, refresh)
	}

	apiHelp := widget.NewLabel(fmt.Sprintf(
		"POST http://%s/notify (or /) — JSON: {\"priority\":3,\"colour\":\"#38BDF8\",\"title\":\"Example\",\"body\":\"Hello\",\"icon\":\"🔔\",\"link\":\"https://example.com\"}",
		*listenAddr,
	))
	apiHelp.Wrapping = fyne.TextWrapWord
	apiHelp.Importance = widget.LowImportance

	priorityHelp := widget.NewLabel("P1 in-app only · P2 + taskbar flash · P3 + Windows notification + sound · P4 repeats for a configurable time (30 s default) · P5 critical sound until acknowledged")
	priorityHelp.Wrapping = fyne.TextWrapWord
	priorityHelp.Importance = widget.LowImportance

	helpBox := container.NewVBox(priorityHelp, apiHelp)
	helpButton := widget.NewButton("Hide help", nil)
	helpButton.Importance = widget.LowImportance

	applyHelpVisibility := func(show bool) {
		if show {
			helpBox.Show()
			helpButton.SetText("Hide help")
		} else {
			helpBox.Hide()
			helpButton.SetText("Show help")
		}
	}

	applyHelpVisibility(state.Settings().ShowHelp)
	helpButton.OnTapped = func() {
		updated := state.Settings()
		updated.ShowHelp = !updated.ShowHelp
		applyHelpVisibility(updated.ShowHelp)
		if err := state.SetSettings(updated); err != nil {
			statusLabel.SetText("Help visibility changed, but settings could not be saved: " + err.Error())
		}
	}

	toolbar := container.NewBorder(
		nil,
		nil,
		pendingLabel,
		container.NewHBox(helpButton, settingsButton, dismissAllButton),
		widget.NewLabel("Pending notifications"),
	)

	card := widget.NewCard(
		"Notification inbox",
		"Notifications remain here until acknowledged.",
		notificationList,
	)

	top := container.NewVBox(toolbar, helpBox, statusLabel)
	w.SetContent(container.NewBorder(top, nil, nil, nil, card))
	refresh(false)

	if loadErr != nil {
		statusLabel.SetText("Loaded with warning: " + loadErr.Error())
	}

	handler := http.NewServeMux()
	handler.HandleFunc("/health", func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			rw.Header().Set("Allow", http.MethodGet)
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "pending": state.Count()})
	})

	notifyHandler := func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			rw.Header().Set("Allow", http.MethodPost)
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !authorisedRequest(r, state.Settings().AuthToken) {
			writeJSON(rw, http.StatusUnauthorized, map[string]any{"error": "unauthorised"})
			return
		}

		r.Body = http.MaxBytesReader(rw, r.Body, maxRequestBody)
		decoder := json.NewDecoder(r.Body)
		var incoming IncomingNotification
		if err := decoder.Decode(&incoming); err != nil {
			if errors.Is(err, io.EOF) {
				writeJSON(rw, http.StatusBadRequest, map[string]any{"error": "empty request body"})
			} else {
				writeJSON(rw, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
			}
			return
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			writeJSON(rw, http.StatusBadRequest, map[string]any{"error": "request body must contain exactly one JSON object"})
			return
		}

		item, err := state.Add(incoming)
		if err != nil {
			writeJSON(rw, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}

		alerts.Start(item)
		fyne.Do(func() { refresh(true) })

		writeJSON(rw, http.StatusAccepted, map[string]any{
			"status":      "accepted",
			"id":          item.ID,
			"received_at": item.ReceivedAt,
		})
	}

	handler.HandleFunc("/notify", notifyHandler)
	handler.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(rw, r)
			return
		}
		notifyHandler(rw, r)
	})

	server := &http.Server{
		Addr:              *listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fyne.Do(func() {
				statusLabel.SetText("HTTP receiver failed: " + err.Error())
				dialog.ShowError(fmt.Errorf("HTTP receiver failed: %w", err), w)
			})
			return
		}
	}()

	if loadErr == nil {
		statusLabel.SetText("Listening on http://" + *listenAddr + "/notify")
	}

	w.ShowAndRun()

	alerts.StopEverything()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func showSettingsDialog(
	w fyne.Window,
	state *State,
	statusLabel *widget.Label,
	refresh func(bool),
) {
	current := state.Settings()

	flashCheck := widget.NewCheck("P2+ taskbar flash", nil)
	flashCheck.SetChecked(current.TaskbarFlash)
	windowsCheck := widget.NewCheck("P3+ Windows notifications", nil)
	windowsCheck.SetChecked(current.WindowsNotifications)
	soundCheck := widget.NewCheck("P3+ sounds", nil)
	soundCheck.SetChecked(current.SoundsEnabled)
	newestFirstCheck := widget.NewCheck("Newest notifications at the top", nil)
	newestFirstCheck.SetChecked(current.NewestFirst)
	persistCheck := widget.NewCheck("Restore pending notifications after restart", nil)
	persistCheck.SetChecked(current.PersistPending)

	normalSoundEntry := widget.NewEntry()
	normalSoundEntry.SetText(current.NormalSoundPath)
	normalSoundEntry.SetPlaceHolder("blank = Windows default alert sound")
	criticalSoundEntry := widget.NewEntry()
	criticalSoundEntry.SetText(current.CriticalSoundPath)
	criticalSoundEntry.SetPlaceHolder("blank = Windows critical alert sound")

	chooseSound := func(target *widget.Entry) {
		picker := dialog.NewFileOpen(func(reader fyne.URIReadCloser, err error) {
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			if reader == nil {
				return
			}
			defer reader.Close()
			target.SetText(localPathFromURI(reader.URI()))
		}, w)
		picker.SetFilter(storage.NewExtensionFileFilter([]string{".mp3", ".wav"}))
		picker.Show()
	}

	normalBrowse := widget.NewButton("Browse…", func() { chooseSound(normalSoundEntry) })
	normalBrowse.Importance = widget.LowImportance
	criticalBrowse := widget.NewButton("Browse…", func() { chooseSound(criticalSoundEntry) })
	criticalBrowse.Importance = widget.LowImportance

	p4SecondsEntry := widget.NewEntry()
	p4SecondsEntry.SetText(strconv.Itoa(current.Priority4Seconds))
	repeatGapEntry := widget.NewEntry()
	repeatGapEntry.SetText(strconv.Itoa(current.RepeatGapMS))
	flashCountEntry := widget.NewEntry()
	flashCountEntry.SetText(strconv.Itoa(current.FlashCount))

	tokenEntry := widget.NewPasswordEntry()
	tokenEntry.SetText(current.AuthToken)
	tokenEntry.SetPlaceHolder("optional; use Bearer token or X-Notify-Token")

	normalTest := widget.NewButton("Test normal", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		go func() {
			defer cancel()
			_ = playSoundOnce(ctx, uint64(time.Now().UnixNano()), 1, strings.TrimSpace(normalSoundEntry.Text), false)
		}()
	})
	normalTest.Importance = widget.LowImportance
	criticalTest := widget.NewButton("Test critical", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		go func() {
			defer cancel()
			_ = playSoundOnce(ctx, uint64(time.Now().UnixNano()), 1, strings.TrimSpace(criticalSoundEntry.Text), true)
		}()
	})
	criticalTest.Importance = widget.LowImportance

	soundRow := func(entry *widget.Entry, browse, test *widget.Button) fyne.CanvasObject {
		return container.NewBorder(nil, nil, nil, container.NewHBox(browse, test), entry)
	}

	hint := widget.NewLabel("Priority behaviour is cumulative. P4 uses the normal sound; P5 uses the critical sound and repeats until that item is acknowledged. MP3 and WAV are supported through Windows MCI.")
	hint.Wrapping = fyne.TextWrapWord
	hint.Importance = widget.LowImportance

	content := container.NewVBox(
		flashCheck,
		windowsCheck,
		soundCheck,
		widget.NewSeparator(),
		widget.NewLabel("Normal alert sound"),
		soundRow(normalSoundEntry, normalBrowse, normalTest),
		widget.NewLabel("Priority 5 sound"),
		soundRow(criticalSoundEntry, criticalBrowse, criticalTest),
		widget.NewSeparator(),
		container.NewBorder(nil, nil, widget.NewLabel("P4 repeat duration (seconds)"), nil, p4SecondsEntry),
		container.NewBorder(nil, nil, widget.NewLabel("Gap between repeats (ms)"), nil, repeatGapEntry),
		container.NewBorder(nil, nil, widget.NewLabel("Taskbar flash count"), nil, flashCountEntry),
		newestFirstCheck,
		persistCheck,
		widget.NewSeparator(),
		widget.NewLabel("Shared HTTP token (recommended when listening beyond localhost)"),
		tokenEntry,
		hint,
	)

	d := dialog.NewCustomConfirm(
		"Notification settings",
		"Save",
		"Cancel",
		content,
		func(save bool) {
			if !save {
				return
			}

			p4Seconds, err := parseIntSetting(p4SecondsEntry.Text, 1, 600, "P4 repeat duration")
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			repeatGap, err := parseIntSetting(repeatGapEntry.Text, 0, 10000, "repeat gap")
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			flashCount, err := parseIntSetting(flashCountEntry.Text, 1, 20, "flash count")
			if err != nil {
				dialog.ShowError(err, w)
				return
			}

			updated := current
			updated.TaskbarFlash = flashCheck.Checked
			updated.WindowsNotifications = windowsCheck.Checked
			updated.SoundsEnabled = soundCheck.Checked
			updated.NormalSoundPath = strings.TrimSpace(normalSoundEntry.Text)
			updated.CriticalSoundPath = strings.TrimSpace(criticalSoundEntry.Text)
			updated.Priority4Seconds = p4Seconds
			updated.RepeatGapMS = repeatGap
			updated.FlashCount = flashCount
			updated.NewestFirst = newestFirstCheck.Checked
			updated.PersistPending = persistCheck.Checked
			updated.AuthToken = strings.TrimSpace(tokenEntry.Text)

			if err := state.SetSettings(updated); err != nil {
				dialog.ShowError(err, w)
				statusLabel.SetText("Could not save settings: " + err.Error())
				return
			}
			refresh(false)
			statusLabel.SetText("Settings saved.")
		},
		w,
	)
	d.Resize(fyne.NewSize(680, 650))
	d.Show()

}

func localPathFromURI(uri fyne.URI) string {
	if uri == nil {
		return ""
	}
	path := filepath.FromSlash(uri.Path())
	// Some file-URI implementations represent C:\... as /C:/...
	if len(path) >= 3 && (path[0] == '/' || path[0] == '\\') && path[2] == ':' {
		path = path[1:]
	}
	return path
}

func parseIntSetting(text string, minimum, maximum int, name string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be from %d to %d", name, minimum, maximum)
	}
	return value, nil
}

func authorisedRequest(r *http.Request, configuredToken string) bool {
	configuredToken = strings.TrimSpace(configuredToken)
	if configuredToken == "" {
		return true
	}

	candidate := strings.TrimSpace(r.Header.Get("X-Notify-Token"))
	if candidate == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
			candidate = strings.TrimSpace(auth[7:])
		}
	}
	if len(candidate) != len(configuredToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(configuredToken)) == 1
}

func writeJSON(rw http.ResponseWriter, status int, value any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(value)
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const storageFile = "reminders.json"

type Reminder struct {
	ID        int64     `json:"id"`
	ChatID    int64     `json:"chat_id"`
	UserID    int64     `json:"user_id"`
	Text      string    `json:"text"`
	RemindAt  time.Time `json:"remind_at"`
	CreatedAt time.Time `json:"created_at"`
	Sent      bool      `json:"sent"`
}

type ReminderStore struct {
	mu        sync.Mutex
	filePath  string
	items     []Reminder
	nextID    int64
	lastDirty bool
}

func NewReminderStore(filePath string) (*ReminderStore, error) {
	store := &ReminderStore{filePath: filePath, items: []Reminder{}, nextID: 1}
	if _, err := os.Stat(filePath); errors.Is(err, os.ErrNotExist) {
		return store, nil
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read storage: %w", err)
	}
	if len(data) == 0 {
		return store, nil
	}

	if err := json.Unmarshal(data, &store.items); err != nil {
		return nil, fmt.Errorf("decode storage: %w", err)
	}
	for _, r := range store.items {
		if r.ID >= store.nextID {
			store.nextID = r.ID + 1
		}
	}
	return store, nil
}

func (s *ReminderStore) saveLocked() error {
	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return fmt.Errorf("encode storage: %w", err)
	}
	if err := os.WriteFile(s.filePath, data, 0o644); err != nil {
		return fmt.Errorf("write storage: %w", err)
	}
	return nil
}

func (s *ReminderStore) Add(chatID, userID int64, text string, remindAt time.Time) (Reminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := Reminder{
		ID:        s.nextID,
		ChatID:    chatID,
		UserID:    userID,
		Text:      text,
		RemindAt:  remindAt,
		CreatedAt: time.Now(),
		Sent:      false,
	}
	s.items = append(s.items, r)
	s.nextID++

	if err := s.saveLocked(); err != nil {
		return Reminder{}, err
	}
	return r, nil
}

func (s *ReminderStore) ListActiveByChat(chatID int64) []Reminder {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]Reminder, 0)
	for _, r := range s.items {
		if r.ChatID == chatID && !r.Sent {
			result = append(result, r)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].RemindAt.Before(result[j].RemindAt)
	})
	return result
}

func (s *ReminderStore) Delete(chatID, reminderID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := -1
	for i, r := range s.items {
		if r.ID == reminderID && r.ChatID == chatID && !r.Sent {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false, nil
	}

	s.items = append(s.items[:idx], s.items[idx+1:]...)
	if err := s.saveLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *ReminderStore) Due(now time.Time) ([]Reminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	due := make([]Reminder, 0)
	changed := false
	for i := range s.items {
		if s.items[i].Sent {
			continue
		}
		if !s.items[i].RemindAt.After(now) {
			s.items[i].Sent = true
			due = append(due, s.items[i])
			changed = true
		}
	}

	if changed {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}

	return due, nil
}

func main() {
	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if token == "" {
		log.Fatal("set TELEGRAM_BOT_TOKEN env variable")
	}

	store, err := NewReminderStore(storageFile)
	if err != nil {
		log.Fatalf("init storage: %v", err)
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Fatalf("create bot: %v", err)
	}

	bot.Debug = false
	log.Printf("authorized as @%s", bot.Self.UserName)

	go runScheduler(bot, store)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}
		if !update.Message.IsCommand() {
			continue
		}

		chatID := update.Message.Chat.ID
		userID := int64(0)
		if update.Message.From != nil {
			userID = int64(update.Message.From.ID)
		}

		resp := handleCommand(store, update.Message.Command(), update.Message.CommandArguments(), chatID, userID)
		msg := tgbotapi.NewMessage(chatID, resp)
		if _, err := bot.Send(msg); err != nil {
			log.Printf("send message: %v", err)
		}
	}
}

func runScheduler(bot *tgbotapi.BotAPI, store *ReminderStore) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for now := range ticker.C {
		due, err := store.Due(now)
		if err != nil {
			log.Printf("scheduler due: %v", err)
			continue
		}

		for _, r := range due {
			text := fmt.Sprintf("⏰ Напоминание #%d\n%s", r.ID, r.Text)
			msg := tgbotapi.NewMessage(r.ChatID, text)
			if _, err := bot.Send(msg); err != nil {
				log.Printf("send reminder #%d: %v", r.ID, err)
			}
		}
	}
}

func handleCommand(store *ReminderStore, command, args string, chatID, userID int64) string {
	switch command {
	case "start", "help":
		return helpText()
	case "in":
		return createInReminder(store, chatID, userID, args)
	case "at":
		return createAtReminder(store, chatID, userID, args)
	case "list":
		return listReminders(store, chatID)
	case "delete":
		return deleteReminder(store, chatID, args)
	default:
		return "Неизвестная команда. Нажми /help"
	}
}

func helpText() string {
	return strings.Join([]string{
		"Я бот-напоминалка.",
		"",
		"Команды:",
		"/in <длительность> <текст> - напомнить через время. Пример: /in 10m Позвонить маме",
		"/at <YYYY-MM-DD HH:MM> <текст> - напомнить в конкретное время. Пример: /at 2026-03-06 21:30 Выпить воду",
		"/list - показать активные напоминания",
		"/delete <id> - удалить напоминание",
	}, "\n")
}

func createInReminder(store *ReminderStore, chatID, userID int64, args string) string {
	parts := strings.Fields(args)
	if len(parts) < 2 {
		return "Формат: /in <длительность> <текст>. Пример: /in 45m Сделать перерыв"
	}

	d, err := time.ParseDuration(parts[0])
	if err != nil || d <= 0 {
		return "Некорректная длительность. Примеры: 30s, 10m, 2h"
	}

	text := strings.TrimSpace(strings.Join(parts[1:], " "))
	if text == "" {
		return "Текст напоминания не должен быть пустым"
	}

	remindAt := time.Now().Add(d)
	r, err := store.Add(chatID, userID, text, remindAt)
	if err != nil {
		log.Printf("add reminder /in: %v", err)
		return "Не получилось сохранить напоминание"
	}

	return fmt.Sprintf("Ок, напомню в %s (id: %d)", remindAt.Format("2006-01-02 15:04"), r.ID)
}

func createAtReminder(store *ReminderStore, chatID, userID int64, args string) string {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return "Формат: /at <YYYY-MM-DD HH:MM> <текст>"
	}

	parts := strings.SplitN(trimmed, " ", 3)
	if len(parts) < 3 {
		return "Формат: /at <YYYY-MM-DD HH:MM> <текст>"
	}

	dtRaw := parts[0] + " " + parts[1]
	text := strings.TrimSpace(parts[2])
	if text == "" {
		return "Текст напоминания не должен быть пустым"
	}

	remindAt, err := time.ParseInLocation("2006-01-02 15:04", dtRaw, time.Local)
	if err != nil {
		return "Не понял дату. Формат: YYYY-MM-DD HH:MM"
	}
	if remindAt.Before(time.Now()) {
		return "Эта дата уже в прошлом. Укажи будущее время"
	}

	r, err := store.Add(chatID, userID, text, remindAt)
	if err != nil {
		log.Printf("add reminder /at: %v", err)
		return "Не получилось сохранить напоминание"
	}

	return fmt.Sprintf("Ок, напомню %s (id: %d)", remindAt.Format("2006-01-02 15:04"), r.ID)
}

func listReminders(store *ReminderStore, chatID int64) string {
	items := store.ListActiveByChat(chatID)
	if len(items) == 0 {
		return "Активных напоминаний нет"
	}

	lines := make([]string, 0, len(items)+1)
	lines = append(lines, "Твои активные напоминания:")
	for _, r := range items {
		lines = append(lines, fmt.Sprintf("%d) %s - %s", r.ID, r.RemindAt.Format("2006-01-02 15:04"), r.Text))
	}
	return strings.Join(lines, "\n")
}

func deleteReminder(store *ReminderStore, chatID int64, args string) string {
	idRaw := strings.TrimSpace(args)
	if idRaw == "" {
		return "Формат: /delete <id>"
	}

	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil || id <= 0 {
		return "ID должен быть положительным числом"
	}

	deleted, err := store.Delete(chatID, id)
	if err != nil {
		log.Printf("delete reminder: %v", err)
		return "Не получилось удалить напоминание"
	}
	if !deleted {
		return "Напоминание не найдено"
	}
	return fmt.Sprintf("Удалено напоминание %d", id)
}

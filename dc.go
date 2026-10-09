package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"
)

var (
	adminPerms = int64(discordgo.PermissionAdministrator)
)

var (
	badWordRegex *regexp.Regexp
	sapaanRegex  *regexp.Regexp
	pamitRegex   *regexp.Regexp
)

var (
	sbTracker    = make(map[string][]time.Time)
	sbTrackerMu  sync.Mutex
	sbSpamLimit  = 5
	sbTimeWindow = 10 * time.Second
)

type Bot struct {
	session      *discordgo.Session
	db           *sql.DB
	dbMu         sync.Mutex
	registerOnce sync.Once
}

func init() {
	badPattern := `(?i)\b(` + strings.Join(kataBuruk, "|") + `)\b`
	badWordRegex = regexp.MustCompile(badPattern)

	sapaanPattern := `(?i)\b(` + strings.Join(kataSapaan, "|") + `)\b`
	sapaanRegex = regexp.MustCompile(sapaanPattern)

	pamitPattern := `(?i)\b(` + strings.Join(kataPamit, "|") + `)\b`
	pamitRegex = regexp.MustCompile(pamitPattern)
}

func main() {
	_ = godotenv.Load()

	token := strings.TrimSpace(os.Getenv("DISCORD_TOKEN"))
	if token == "" {
		log.Println("Token tidak ditemukan! Pastikan file .env sudah benar.")
		return
	}

	db, err := initDatabase(filepath.Join(".", databaseFileName))
	if err != nil {
		log.Fatalf("gagal inisialisasi database: %v", err)
	}
	defer db.Close()

	session, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("gagal membuat session discord: %v", err)
	}

	session.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsGuildMembers |
		discordgo.IntentsGuildVoiceStates |
		discordgo.IntentsMessageContent

	bot := &Bot{
		session: session,
		db:      db,
	}

	session.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		s.UpdateGameStatus(0, "g!aturan")
	})

	session.AddHandler(bot.onReady)
	session.AddHandler(bot.onInteractionCreate)
	session.AddHandler(bot.onGuildMemberAdd)
	session.AddHandler(bot.onMessageCreate)
	session.AddHandler(bot.onSoundboardSpamCheck)

	bot.startAutoDisconnectChecker(session)
	bot.startDeadlineChecker(session)
	bot.startAutoPromotionChecker(session)
	bot.startSoundboardCleaner()

	session.State.MaxMessageCount = 10

	if err := session.Open(); err != nil {
		log.Fatalf("gagal membuka koneksi discord: %v", err)
	}
	defer session.Close()

	log.Println("Bot berjalan. Tekan CTRL+C untuk keluar.")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-stop

	log.Println("Menutup bot...")
}

func initDatabase(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=cache_size(2000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS undian (
			guild_id TEXT,
			user_id TEXT,
			is_picked BOOLEAN DEFAULT 0,
			last_active DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return nil, err
	}

	if _, err := db.Exec(`ALTER TABLE undian ADD COLUMN last_active DATETIME DEFAULT CURRENT_TIMESTAMP`); err != nil {
		lowerErr := strings.ToLower(err.Error())
		if !strings.Contains(lowerErr, "duplicate column name") {
			return nil, err
		}
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			user_id INTEGER PRIMARY KEY,
			msg_count INTEGER DEFAULT 0,
			points INTEGER DEFAULT 0
		)
	`); err != nil {
		return nil, err
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS deadlines (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT,
			channel_id TEXT,
			tugas TEXT,
			waktu DATETIME,
			is_public BOOLEAN DEFAULT 0
		)
	`); err != nil {
		return nil, err
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS bot_access (
			user_id TEXT PRIMARY KEY
		)
	`); err != nil {
		return nil, err
	}

	if _, err := db.Exec(`ALTER TABLE deadlines ADD COLUMN is_public BOOLEAN DEFAULT 0`); err != nil {
		lowerErr := strings.ToLower(err.Error())
		if !strings.Contains(lowerErr, "duplicate column name") {
			return nil, err
		}
	}

	return db, nil
}

func (b *Bot) onGuildMemberAdd(s *discordgo.Session, member *discordgo.GuildMemberAdd) {
	pesanSapaan := fmt.Sprintf("Halo <@%s>, selamat datang di server GaCor! 🍻 *Have fun!*", member.User.ID)

	_, err := s.ChannelMessageSend(channelChatLobby, pesanSapaan)
	if err != nil {
		log.Printf("Gagal mengirim pesan sambutan untuk %s: %v", member.User.Username, err)
	}
}

func (b *Bot) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		b.handleApplicationCommand(s, i)

	case discordgo.InteractionMessageComponent:
		b.handleMessageComponent(s, i)
	}
}

func (b *Bot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || m.GuildID == "" {
		return
	}

	content := strings.ToLower(m.Content)
	isToxic := containsAny(content, kataBuruk)

	oldPoints, newPoints, err := b.recordMessage(m.Author.ID, isToxic)
	if err != nil {
		log.Printf("gagal update database user %s: %v", m.Author.ID, err)
		return
	}

	if isToxic {
		if _, err := s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("⚠️ Waduh <@%s>, bahasanya dijaga ya! Poin dikurangi 1.", m.Author.ID)); err != nil {
			log.Printf("gagal mengirim warning toxic: %v", err)
		}

		if err := b.sendAdminLog(s, m); err != nil {
			log.Printf("gagal mengirim log admin: %v", err)
		}
	}

	oldTier := karmaRoleNameForPoints(oldPoints)
	newTier := karmaRoleNameForPoints(newPoints)

	if oldTier != newTier {
		if err := b.updateKarmaRole(s, m.GuildID, m.Author.ID, newPoints); err != nil {
			log.Printf("gagal update karma role: %v", err)
		}
	}

	switch {
	case sapaanRegex.MatchString(content):
		_, _ = s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Halo, Bro %s 😎 !", displayNameFromMessage(m)))
	case pamitRegex.MatchString(content):
		_, _ = s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Ogey, %s!, bye👋", displayNameFromMessage(m)))
	case content == commandPing:
		_, _ = s.ChannelMessageSend(m.ChannelID, "CIIIHUUUYYY!!!")
	case content == commandLogin:
		_, _ = s.ChannelMessageSend(m.ChannelID, "@everyone Sudah Waktunya Log in.")
	case content == commandRules:
		isAdmin, _ := b.memberHasRoleByName(s, m.GuildID, m.Author.ID, adminRoleName)
		isMagang, _ := b.memberHasRoleByName(s, m.GuildID, m.Author.ID, magangRoleName)

		if !isAdmin && !isMagang {
			return
		}
		_, err := s.ChannelMessageSendEmbed(m.ChannelID, b.getRulesEmbed())
		if err != nil {
			log.Printf("Gagal mengirim embed rules via chat: %v", err)
		}
	}
}

func (b *Bot) onReady(s *discordgo.Session, ready *discordgo.Ready) {
	log.Printf("Bot %s online!", ready.User.String())

	b.registerOnce.Do(func() {
		commands, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, guildID, applicationCommands())
		if err != nil {
			log.Printf("Error sinkronisasi command: %v", err)
			return
		}

		log.Printf("Sinkronisasi %d command slash berhasil di guild %s!", len(commands), guildID)
	})
}

func (b *Bot) onSoundboardSpamCheck(s *discordgo.Session, e *discordgo.Event) {
	if e.Type == "VOICE_CHANNEL_EFFECT_SEND" {
		log.Println("🚨 [DEBUG] Sinyal Soundboard terdeteksi dari Discord!")

		var data struct {
			UserID    string `json:"user_id"`
			GuildID   string `json:"guild_id"`
			ChannelID string `json:"channel_id"`
		}
		if err := json.Unmarshal(e.RawData, &data); err != nil {
			log.Printf("❌ [DEBUG] Gagal bongkar JSON: %v\n", err)
			return
		}
		if data.UserID == "" || data.ChannelID == "" {
			pesanError := "⚠️ **[SYSTEM ALERT]** Deteksi Soundboard gagal! Struktur API Discord sepertinya berubah. Tolong admin cek ulang kodenya."
			_, _ = s.ChannelMessageSend(channelLogAdmin, pesanError)

			log.Println("⚠️ [WARNING] Struktur API Discord berubah! UserID atau ChannelID kosong.")
			return
		}

		log.Printf("👀 [DEBUG] Si <@%s> mencet soundboard di channel %s\n", data.UserID, data.ChannelID)

		sbTrackerMu.Lock()
		defer sbTrackerMu.Unlock()

		now := time.Now()
		var validTimes []time.Time

		for _, t := range sbTracker[data.UserID] {
			if now.Sub(t) <= sbTimeWindow {
				validTimes = append(validTimes, t)
			}
		}

		validTimes = append(validTimes, now)
		sbTracker[data.UserID] = validTimes
		log.Printf("📊 [DEBUG] Total pencetan si <@%s> dalam 10 dtk terakhir: %d kali\n", data.UserID, len(validTimes))

		if len(validTimes) >= sbSpamLimit {
			log.Println("⚔️ [DEBUG] LIMIT TERCAPAI! Mengeksekusi Combo Maut...")

			timeoutWaktu := now.Add(1 * time.Minute)
			err := s.GuildMemberTimeout(data.GuildID, data.UserID, &timeoutWaktu)
			if err != nil {
				log.Printf("❌ [DEBUG] Gagal Timeout: %v\n", err)
			} else {
				log.Println("✅ [DEBUG] Timeout 1 menit berhasil!")
			}

			newPoints, err := b.addPoints(data.UserID, -2)
			if err != nil {
				log.Printf("❌ [DEBUG] Gagal potong poin: %v\n", err)
			} else {
				log.Println("✅ [DEBUG] Potong poin berhasil!")
			}

			_ = b.updateKarmaRole(s, data.GuildID, data.UserID, newPoints)

			pesanTilang := fmt.Sprintf("🚨 **TETOT!** <@%s> kena tilang Timeout 1 menit karena spam Soundboard! Poin dikurangi 2 (Sisa poin: %d).", data.UserID, newPoints)
			_, errVC := s.ChannelMessageSend(data.ChannelID, pesanTilang)
			if errVC != nil {
				log.Printf("❌ [DEBUG] Gagal ngirim pesan ke VC: %v\n", errVC)
			}

			pesanLog := fmt.Sprintf("📋 **LOG SYSTEM**: <@%s> telah di-timeout otomatis 1 menit karena Spam Soundboard di <#%s>", data.UserID, data.ChannelID)
			_, errLog := s.ChannelMessageSend(channelLogAdmin, pesanLog)
			if errLog != nil {
				log.Printf("❌ [DEBUG] Gagal ngirim pesan ke Log Admin: %v\n", errLog)
			}

			delete(sbTracker, data.UserID)
		}
	}
}

func applicationCommands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        "info_bot",
			Description: "Lihat informasi, panduan, dan daftar fitur bot ROZON",
		},
		{
			Name:        "poin",
			Description: "Cek dan lihat peringkat poin server",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "aksi",
					Description: "Pilih aksi yang mau dilakukan",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "Cek Poin Pribadi", Value: "cek"},
						{Name: "Lihat Top Poin", Value: "top"},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "tampilkan",
					Description: "Tampilkan secara Publik atau Pribadi? (Khusus Top Poin)",
					Required:    false,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "Privat (Hanya aku)", Value: "privat"},
						{Name: "Publik (Semua orang - Khusus Admin)", Value: "publik"},
					},
				},
			},
		},
		{
			Name:                     "admin_poin",
			Description:              "Manajemen Poin (Khusus Admin Besar Asli)",
			DefaultMemberPermissions: &adminPerms,
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "aksi",
					Description: "Pilih aksi poin",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "Tambah Poin", Value: "beri"},
						{Name: "Kurangi Poin", Value: "kurangi"},
					},
				},
				{Type: discordgo.ApplicationCommandOptionUser, Name: "member", Description: "Member tujuan", Required: true},
				{Type: discordgo.ApplicationCommandOptionInteger, Name: "jumlah", Description: "Jumlah poin", Required: true},
			},
		},
		{
			Name:                     "admin_role",
			Description:              "Manajemen Role Member (Khusus Admin Besar Asli)",
			DefaultMemberPermissions: &adminPerms,
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "aksi",
					Description: "Pilih aksi role",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "Berikan Role", Value: "tambah"},
						{Name: "Cabut Role", Value: "cabut"},
						{Name: "Lihat Role Member", Value: "lihat"},
					},
				},
				{Type: discordgo.ApplicationCommandOptionUser, Name: "member", Description: "Member tujuan", Required: true},
				{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "Role yang dikelola (Opsional untuk aksi Lihat)", Required: false},
			},
		},
		{
			Name:                     "akses_bot",
			Description:              "Manajemen akses kelola bot ke admin magang (Khusus Admin Besar Asli)",
			DefaultMemberPermissions: &adminPerms,
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "aksi",
					Description: "Pilih aksi akses",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "Berikan Akses Bot", Value: "beri"},
						{Name: "Cabut Akses Bot", Value: "cabut"},
						{Name: "Lihat Daftar Admin Magang", Value: "list"},
					},
				},
				{Type: discordgo.ApplicationCommandOptionUser, Name: "member", Description: "Member tujuan (Kosongkan jika memilih aksi List)", Required: false},
			},
		},
		{
			Name:        "hapus_channel",
			Description: "Hapus channel ini atau channel lain (Khusus Admin Besar)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionChannel,
					Name:        "channel",
					Description: "Pilih channel yang mau dihapus (Kosongkan jika channel ini)",
					Required:    false,
				},
			},
		},
		{
			Name:        "bersihkan",
			Description: "Sapu jagat chat di channel ini (Akses Admin & Magang)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "mode",
					Description: "Hapus dari yang terbaru atau terlama?",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "Pesan Terlama (Paling atas)", Value: "terlama"},
						{Name: "Pesan Terbaru (Paling bawah)", Value: "terbaru"},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "jumlah",
					Description: "Jumlah chat yang mau dihapus (Maksimal 100)",
					Required:    true,
				},
			},
		},
		{
			Name:        "admin_chat",
			Description: "Manajemen Chat & UI (Akses Admin & Magang)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "aksi",
					Description: "Pilih menu yang mau dimunculkan",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "Setup Tiket Bantuan", Value: "setup_tiket"},
						{Name: "Kirim Pesan Printer", Value: "printer"},
						{Name: "Panggil Satpam Anti-Spam", Value: "panggil_satpam"},
						{Name: "Usir Satpam Anti-Spam", Value: "usir_satpam"},
					},
				},
				{Type: discordgo.ApplicationCommandOptionString, Name: "teks_printer", Description: "Teks untuk printer (Opsional, khusus aksi printer)", Required: false},
			},
		},
		{
			Name:        "pengingat",
			Description: "Atur tenggat waktu atau jadwal",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "aksi",
					Description: "Pilih aksi pengingat",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "Buat Pengingat Baru (Khusus Admin)", Value: "tambah"},
						{Name: "Lihat Daftar Pengingat", Value: "list"},
						{Name: "Hapus Pengingat (Khusus Admin)", Value: "hapus"},
					},
				},
				{Type: discordgo.ApplicationCommandOptionString, Name: "tugas", Description: "Nama tugas/kegiatan (Khusus Tambah)", Required: false},
				{Type: discordgo.ApplicationCommandOptionString, Name: "waktu", Description: "Format: DD-MM-YYYY HH:MM (Khusus Tambah)", Required: false},
				{Type: discordgo.ApplicationCommandOptionInteger, Name: "id", Description: "ID Pengingat (Khusus Hapus)", Required: false},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "tampilkan",
					Description: "Tampilkan List Publik/Privat? (Khusus List - Publik cuma buat Admin)",
					Required:    false,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "Privat (Hanya aku)", Value: "privat"},
						{Name: "Publik (Semua orang)", Value: "publik"},
					},
				},
			},
		},
		{
			Name:        "undian",
			Description: "Sistem undian (Acak urutan langsung)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "peserta",
					Description: "Tag semua peserta (contoh: @A @B @C)",
					Required:    true,
				},
			},
		},
	}
}

func (b *Bot) handleAddRole(s *discordgo.Session, i *discordgo.InteractionCreate, options []*discordgo.ApplicationCommandInteractionDataOption) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})

	memberID := optionIDValue(optionByName(options, "member"))
	roleID := optionIDValue(optionByName(options, "role"))
	if memberID == "" || roleID == "" {
		content := "❌ Data member atau role tidak valid."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	member, err := s.GuildMember(i.GuildID, memberID)
	if err != nil {
		content := "❌ Member tidak ditemukan."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	role, _ := b.roleByID(s, i.GuildID, roleID)
	roleName := roleID
	if role != nil {
		roleName = role.Name
	}

	if containsString(member.Roles, roleID) {
		content := fmt.Sprintf("Si %s udah punya role %s kok!", displayNameFromMember(member), roleName)
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	if containsString(roleTingkatan, roleName) {
		for _, rName := range roleTingkatan {
			if rName == roleName {
				continue
			}

			oldRole, _ := b.roleByName(s, i.GuildID, rName)
			if oldRole != nil && containsString(member.Roles, oldRole.ID) {
				_ = s.GuildMemberRoleRemove(i.GuildID, memberID, oldRole.ID)
			}
		}
	}

	if err := s.GuildMemberRoleAdd(i.GuildID, memberID, roleID); err != nil {
		if isForbidden(err) {
			content := "❌ Gagal! Pastikan posisi role bot ADA DI ATAS role yang mau dikasih di Server Settings!"
			_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
			return
		}
		content := "❌ Gagal menambahkan role."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	content := fmt.Sprintf("✅ Berhasil memberikan role **%s** ke <@%s>! Cihuy!", roleName, memberID)
	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
}

func (b *Bot) handleAdminChat(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !b.requireAdminOrAccess(s, i) {
		return
	}

	aksi := optionStringValue(optionByName(i.ApplicationCommandData().Options, "aksi"))
	go b.logMagangAction(s, i, "/admin_chat "+aksi)

	switch aksi {
	case "setup_tiket":
		b.handleSetupTicket(s, i)
	case "printer":
		b.handlePrinter(s, i)
	case "panggil_satpam":
		b.handlePanggilBotAntispam(s, i)
	case "usir_satpam":
		b.handleUsirBotAntispam(s, i)
	}
}

func (b *Bot) handleAdminPoin(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !b.requireAdminOrAccess(s, i) {
		return
	}

	aksi := optionStringValue(optionByName(i.ApplicationCommandData().Options, "aksi"))
	switch aksi {
	case "beri":
		b.handleGivePoints(s, i, i.ApplicationCommandData().Options)
	case "kurangi":
		b.handleReducePoints(s, i, i.ApplicationCommandData().Options)
	}
}

func (b *Bot) handleAdminRole(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !b.requireAdminBesarOnly(s, i) {
		return
	}

	aksi := optionStringValue(optionByName(i.ApplicationCommandData().Options, "aksi"))
	switch aksi {
	case "tambah":
		b.handleAddRole(s, i, i.ApplicationCommandData().Options)
	case "cabut":
		b.handleRemoveRole(s, i, i.ApplicationCommandData().Options)
	case "lihat":
		b.handleListMemberRoles(s, i, i.ApplicationCommandData().Options)
	}
}

func (b *Bot) handleAksesBot(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !b.requireAdminBesarOnly(s, i) {
		return
	}

	aksi := optionStringValue(optionByName(i.ApplicationCommandData().Options, "aksi"))
	switch aksi {
	case "beri":
		b.handleGiveAccess(s, i, i.ApplicationCommandData().Options)
	case "cabut":
		b.handleRemoveAccess(s, i, i.ApplicationCommandData().Options)
	case "list":
		b.handleListAccess(s, i)
	}
}

func (b *Bot) handleApplicationCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()

	switch data.Name {
	case "poin":
		b.handlePoin(s, i)
	case "admin_poin":
		b.handleAdminPoin(s, i)
	case "admin_chat":
		b.handleAdminChat(s, i)
	case "admin_role":
		b.handleAdminRole(s, i)
	case "akses_bot":
		b.handleAksesBot(s, i)
	case "info_bot":
		b.handleEmbedInfo(s, i)
	case "hapus_channel":
		if !b.requireAdminBesarOnly(s, i) {
			return
		}
		b.handleDeleteChannel(s, i)
	case "pengingat":
		b.handlePengingat(s, i)
	case "bersihkan":
		b.handleBersihkan(s, i)
	case "undian":
		b.handleUndian(s, i)
	case "panggil_bot_antispam":
		b.handlePanggilBotAntispam(s, i)
	case "usir_bot_antispam":
		b.handleUsirBotAntispam(s, i)
	}
}

func (b *Bot) handleBersihkan(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !b.requireAdminOrAccess(s, i) {
		return
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})

	mode := optionStringValue(optionByName(i.ApplicationCommandData().Options, "mode"))
	jumlahOpt := optionByName(i.ApplicationCommandData().Options, "jumlah")
	if jumlahOpt == nil {
		return
	}
	jumlah := int(jumlahOpt.IntValue())

	if jumlah < 1 || jumlah > 100 {
		content := "❌ Jumlahnya harus antara 1 sampai 100 pesan aja Chuy!"
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	var messages []*discordgo.Message
	var err error

	if mode == "terbaru" {
		messages, err = s.ChannelMessages(i.ChannelID, jumlah, "", "", "")
	} else {
		messages, err = s.ChannelMessages(i.ChannelID, jumlah, "", "1", "")
	}

	if err != nil || len(messages) == 0 {
		content := "❌ Gagal ngambil pesan atau emang udah kosong channelnya."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	var bulkDeleteIDs []string
	var oldMessageIDs []string
	fourteenDaysAgo := time.Now().AddDate(0, 0, -14)

	for _, msg := range messages {
		if msg.Timestamp.Before(fourteenDaysAgo) {
			oldMessageIDs = append(oldMessageIDs, msg.ID)
		} else {
			bulkDeleteIDs = append(bulkDeleteIDs, msg.ID)
		}
	}

	if len(bulkDeleteIDs) > 0 {
		err = s.ChannelMessagesBulkDelete(i.ChannelID, bulkDeleteIDs)
		if err != nil {
			log.Printf("Gagal bulk delete: %v", err)
		}
	}

	if len(oldMessageIDs) > 0 {
		for _, id := range oldMessageIDs {
			_ = s.ChannelMessageDelete(i.ChannelID, id)
			time.Sleep(50 * time.Millisecond)
		}
	}

	go b.logMagangAction(s, i, fmt.Sprintf("/bersihkan %s %d pesan", mode, jumlah))

	content := fmt.Sprintf("🧹 Cihuy! Berhasil menyapu bersih **%d** pesan (%s) dari channel ini!", len(messages), mode)
	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
}

func (b *Bot) handleCheckPoints(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Member == nil || i.Member.User == nil {
		respondEphemeral(s, i, "❌ Data member tidak ditemukan.")
		return
	}

	msgCount, points, err := b.getUserStats(i.Member.User.ID)
	if err != nil {
		respondEphemeral(s, i, "❌ Yah, data poinnya gagal dibaca.")
		return
	}

	content := fmt.Sprintf("Wih, kamu sudah chat sebanyak %d kali di server ini dan total poinmu ada %d!", msgCount, points)
	respondEphemeral(s, i, content)
}

func (b *Bot) handleCloseTicketButton(s *discordgo.Session, i *discordgo.InteractionCreate) {
	respond(s, i, "Tiket akan ditutup dan dihapus dalam 5 detik... ", false)

	channelID := i.ChannelID
	go func() {
		time.Sleep(5 * time.Second)
		if _, err := s.ChannelDelete(channelID); err != nil && !isForbidden(err) {
			log.Printf("gagal hapus tiket: %v", err)
		}
	}()
}

func (b *Bot) handleCreateTicketButton(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})

	if i.Member == nil || i.Member.User == nil {
		content := "❌ Data member tidak ditemukan."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	adminRole, err := b.roleByName(s, i.GuildID, adminRoleName)
	if err != nil || adminRole == nil {
		content := "Waduh, role admin tidak ditemukan. Coba lapor langsung ke admin!"
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	channelName := "tiket-" + sanitizeTextChannelName(displayNameFromInteraction(i))
	channels, err := s.GuildChannels(i.GuildID)
	if err == nil {
		for _, channel := range channels {
			if channel.Type == discordgo.ChannelTypeGuildText && channel.Name == channelName {
				content := fmt.Sprintf("Eits, kamu masih punya tiket yang belum ditutup di <#%s>! Selesaikan dulu yang itu ya.", channel.ID)
				_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
				return
			}
		}
	}

	ticketChannel, err := s.GuildChannelCreateComplex(i.GuildID, discordgo.GuildChannelCreateData{
		Name: channelName,
		Type: discordgo.ChannelTypeGuildText,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: i.GuildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel},
			{ID: i.Member.User.ID, Type: discordgo.PermissionOverwriteTypeMember, Allow: discordgo.PermissionViewChannel | discordgo.PermissionSendMessages},
			{ID: adminRole.ID, Type: discordgo.PermissionOverwriteTypeRole, Allow: discordgo.PermissionViewChannel | discordgo.PermissionSendMessages},
		},
	})
	if err != nil {
		log.Printf("gagal membuat tiket: %v", err)
		content := "❌ Tiket gagal dibuat."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	content := fmt.Sprintf("✅ Tiket berhasil dibuat! Silakan ke <#%s>", ticketChannel.ID)
	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})

	_, _ = s.ChannelMessageSendComplex(ticketChannel.ID, &discordgo.MessageSend{
		Content:    fmt.Sprintf("Halo <@%s>, silakan sampaikan masalahmu di sini. Klik tombol di bawah jika urusan sudah selesai ya!", i.Member.User.ID),
		Components: closeTicketButtonComponents(),
	})
}

func (b *Bot) handleDeleteChannel(s *discordgo.Session, i *discordgo.InteractionCreate) {
	channelID := i.ChannelID
	channelOpt := optionByName(i.ApplicationCommandData().Options, "channel")
	if channelOpt != nil {
		if selectedID := optionIDValue(channelOpt); selectedID != "" {
			channelID = selectedID
		}
	}

	respond(s, i, fmt.Sprintf("🚨 Channel <#%s> akan dihapus dalam 3 detik... Bye bye!", channelID), false)

	go func() {
		time.Sleep(3 * time.Second)
		if _, err := s.ChannelDelete(channelID); err != nil && !isForbidden(err) {
			log.Printf("gagal hapus channel %s: %v", channelID, err)
		}
	}()
}

func (b *Bot) handleEmbedInfo(s *discordgo.Session, i *discordgo.InteractionCreate) {
	embed := &discordgo.MessageEmbed{
		Title:       "(BOT ADMIN) ROZON",
		URL:         "https://chat.whatsapp.com/BNgNMOYgE2gJSpWV8C4fqv",
		Description: "Saya bot yang diutus untuk sedikit mengelola server ini",
		Color:       0x992D22,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Fitur", Value: "Gunakan `/` untuk menggunakan beberapa fitur yang ada.", Inline: false},
			{
				Name: "💬 Perintah Member",
				Value: "- `/poin` : Cek poin & lihat peringkat member\n" +
					"- `/info_bot` : Info lengkap panduan & fitur bot\n" +
					"- `/pengingat` : Lihat jadwal atau deadline tugas (Privat)\n" +
					"- `/undian` : Acak urutan peserta undian",

				Inline: false,
			},
			{
				Name: "👑 Admin Magang",
				Value: "- Semua perintah member di atas\n" +
					"- `/admin_chat` : Setup tiket, printer, & **Panggil/Usir Satpam Anti-Spam**\n" +
					"- `/pengingat` : Tambah, Hapus, dan nampilin jadwal secara publik\n" +
					"- `/bersihkan` : Sapu jagat hapus chat massal",
				Inline: false,
			},
			{
				Name: "🏆 Admin Besar Only",
				Value: "- Semua perintah di atas\n" +
					"- `/admin_poin` : Kasih atau kurangi poin member (Akses Penuh)\n" +
					"- `/akses_bot` : Kasih hak admin magang ke member lain\n" +
					"- `/admin_role` : Kasih atau cabut role manual dari member\n" +
					"- `/hapus_channel` : Hapus channel mana pun yang kamu mau",
				Inline: false,
			},
			{
				Name:   "Feedback",
				Value:  "Jika kamu memiliki feedback atau saran, silakan kirimkan melalui tiket bantuan.",
				Inline: false,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Have a Good Day, Sleeping Well!",
		},
	}

	if i.Member != nil && i.Member.User != nil {
		embed.Author = &discordgo.MessageEmbedAuthor{
			Name:    i.Member.User.Username,
			IconURL: i.Member.User.AvatarURL(""),
		}
	}

	logoPath := filepath.Join(".", logoFileName)
	file, err := os.Open(logoPath)
	if err == nil {
		defer file.Close()
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: "attachment://" + logoFileName}
		err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{embed},
				Files: []*discordgo.File{
					{Name: logoFileName, Reader: file},
				},
			},
		})
		if err == nil {
			return
		}

		log.Printf("gagal kirim embed dengan logo, fallback tanpa file: %v", err)
	}

	respondWithEmbed(s, i, embed, false)
}

func (b *Bot) handleGiveAccess(s *discordgo.Session, i *discordgo.InteractionCreate, options []*discordgo.ApplicationCommandInteractionDataOption) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})

	memberID := optionIDValue(optionByName(options, "member"))
	if memberID == "" {
		content := "❌ Kamu harus milih member-nya dulu kalau mau ngasih akses!"
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	isAdminBesar, _ := b.memberHasRoleByName(s, i.GuildID, memberID, adminRoleName)
	if isAdminBesar {
		content := "❌ Dia kan udah Admin Besar bawaan Discord!"
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	b.dbMu.Lock()
	var exists int
	err := b.db.QueryRow(`SELECT 1 FROM bot_access WHERE user_id = ?`, memberID).Scan(&exists)
	if err == nil {
		b.dbMu.Unlock()
		content := "❌ Anak ini sudah punya akses di database."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	_, err = b.db.Exec(`INSERT INTO bot_access (user_id) VALUES (?)`, memberID)
	b.dbMu.Unlock()

	if err != nil {
		content := "❌ Gagal menulis ke database."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	role, err := b.roleByName(s, i.GuildID, magangRoleName)
	if err == nil && role != nil {
		_ = s.GuildMemberRoleAdd(i.GuildID, memberID, role.ID)
	}

	content := fmt.Sprintf("✅ Berhasil! <@%s> sudah masuk database dan diberi role **%s**.", memberID, magangRoleName)
	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
}

func (b *Bot) handleGivePoints(s *discordgo.Session, i *discordgo.InteractionCreate, options []*discordgo.ApplicationCommandInteractionDataOption) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})

	memberID := optionIDValue(optionByName(options, "member"))
	jumlahOpt := optionByName(options, "jumlah")
	if memberID == "" || jumlahOpt == nil {
		content := "❌ Data member atau jumlah poin nggak valid."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	jumlah := jumlahOpt.IntValue()
	member, err := s.GuildMember(i.GuildID, memberID)
	if err != nil {
		content := "❌ Member tidak ditemukan."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	newPoints, err := b.addPoints(memberID, jumlah)
	if err != nil {
		content := "❌ Gagal menambahkan poin ke database."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	if err := b.updateKarmaRole(s, i.GuildID, memberID, newPoints); err != nil {
		log.Printf("gagal update karma role setelah beri_poin: %v", err)
	}

	content := fmt.Sprintf("Berhasil menambahkan %d poin kepada %s!", jumlah, displayNameFromMember(member))
	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
}

func (b *Bot) handleListAccess(s *discordgo.Session, i *discordgo.InteractionCreate) {
	b.dbMu.Lock()
	rows, err := b.db.Query(`SELECT user_id FROM bot_access`)
	b.dbMu.Unlock()

	if err != nil {
		respondEphemeral(s, i, "❌ Gagal menarik data admin magang.")
		return
	}
	defer rows.Close()

	var members []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err == nil {
			members = append(members, fmt.Sprintf("• <@%s>", uid))
		}
	}

	if len(members) == 0 {
		respondEphemeral(s, i, "📋 **Daftar Admin Magang:**\nBelum ada anak magang saat ini.")
		return
	}

	respondEphemeral(s, i, fmt.Sprintf("📋 **Daftar Admin Magang saat ini:**\n%s", strings.Join(members, "\n")))
}

func (b *Bot) handleListMemberRoles(s *discordgo.Session, i *discordgo.InteractionCreate, options []*discordgo.ApplicationCommandInteractionDataOption) {
	memberID := optionIDValue(optionByName(options, "member"))
	if memberID == "" {
		respondEphemeral(s, i, "❌ Pilih member-nya dulu, Zon!")
		return
	}

	member, err := s.GuildMember(i.GuildID, memberID)
	if err != nil {
		respondEphemeral(s, i, "❌ Gagal ngambil data member. Mungkin dia udah left?")
		return
	}

	if len(member.Roles) == 0 {
		respondEphemeral(s, i, fmt.Sprintf("Member <@%s> nggak punya role apa-apa selain @everyone.", memberID))
		return
	}

	var roleList []string
	for _, roleID := range member.Roles {
		roleList = append(roleList, fmt.Sprintf("<@&%s>", roleID))
	}

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🎭 Daftar Role: %s", displayNameFromMember(member)),
		Description: strings.Join(roleList, "\n"),
		Color:       0x3498DB,
		Thumbnail: &discordgo.MessageEmbedThumbnail{
			URL: member.User.AvatarURL("128"),
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("Total: %d Role", len(member.Roles)),
		},
	}

	respondWithEmbed(s, i, embed, true)
}

func (b *Bot) handleMessageComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()

	switch data.CustomID {
	case "tutup_tiket":
		b.handleCloseTicketButton(s, i)
	case "tiket_bantuan":
		b.handleCreateTicketButton(s, i)
	}
}

func (b *Bot) handlePanggilBotAntispam(s *discordgo.Session, i *discordgo.InteractionCreate) {
	respondDeferred(s, i)

	if !b.requireAdminOrAccess(s, i) {
		msg := "❌ Kamu tidak memiliki izin akses."
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
		return
	}

	vsUser, err := s.State.VoiceState(i.GuildID, i.Member.User.ID)
	if err != nil {
		msg := "❌ Kamu harus masuk ke Voice Channel terlebih dahulu."
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
		return
	}

	botState, err := s.State.VoiceState(i.GuildID, s.State.User.ID)
	if err == nil && botState != nil && botState.ChannelID != "" {
		if botState.ChannelID == vsUser.ChannelID {
			msg := "🛡️ Aku sudah di sini menjaga channel ini."
			s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
		} else {
			msg := fmt.Sprintf("🛡️ Aku sedang menjaga channel lain (<#%s>). Usir aku dulu jika ingin pindah.", botState.ChannelID)
			s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
		}
		return
	}

	err = s.ChannelVoiceJoinManual(i.GuildID, vsUser.ChannelID, false, true)
	if err != nil {
		msg := fmt.Sprintf("❌ Gagal masuk Voice Channel: %v", err)
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
		return
	}

	msg := fmt.Sprintf("🛡️ Bot Anti-Spam siap! Aku sedang memantau di <#%s>.", vsUser.ChannelID)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
}

func (b *Bot) handlePengingat(s *discordgo.Session, i *discordgo.InteractionCreate) {
	aksi := optionStringValue(optionByName(i.ApplicationCommandData().Options, "aksi"))

	switch aksi {
	case "tambah":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
		})

		if !b.hasAdminAccess(s, i) {
			content := "❌ DITOLAK! Kamu bukan Admin Besar atau Admin Magang. Nggak boleh nyentuh tombol ini!"
			_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
			return
		}

		go b.logMagangAction(s, i, "/pengingat tambah")
		b.tambahPengingat(s, i, i.ApplicationCommandData().Options)
	case "list":
		b.listPengingat(s, i, i.ApplicationCommandData().Options)
	case "hapus":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
		})

		if !b.hasAdminAccess(s, i) {
			content := "❌ DITOLAK! Kamu bukan Admin Besar atau Admin Magang. Nggak boleh nyentuh tombol ini!"
			_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
			return
		}

		go b.logMagangAction(s, i, "/pengingat hapus")
		b.hapusPengingat(s, i, i.ApplicationCommandData().Options)
	}
}

func (b *Bot) handlePoin(s *discordgo.Session, i *discordgo.InteractionCreate) {
	aksi := optionStringValue(optionByName(i.ApplicationCommandData().Options, "aksi"))
	switch aksi {
	case "cek":
		b.handleCheckPoints(s, i)
	case "top":
		b.handleTopPoints(s, i)
	}
}

func (b *Bot) handlePrinter(s *discordgo.Session, i *discordgo.InteractionCreate) {
	teks := optionStringValue(optionByName(i.ApplicationCommandData().Options, "teks_printer"))
	if teks == "" {
		respondEphemeral(s, i, "❌ Teks printer tidak boleh kosong. Isi opsi teks_printer-nya ya!")
		return
	}

	isiPesan := fmt.Sprintf("```ansi\n\u001b[1;33m%s\u001b[0m\n```", teks)
	respond(s, i, isiPesan, false)
}

func (b *Bot) handleReducePoints(s *discordgo.Session, i *discordgo.InteractionCreate, options []*discordgo.ApplicationCommandInteractionDataOption) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})

	memberID := optionIDValue(optionByName(options, "member"))
	jumlahOpt := optionByName(options, "jumlah")
	if memberID == "" || jumlahOpt == nil {
		content := "❌ Data member atau jumlah poin nggak valid."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	jumlah := jumlahOpt.IntValue()
	potongan := int64(-jumlah)

	member, err := s.GuildMember(i.GuildID, memberID)
	if err != nil {
		content := "❌ Member tidak ditemukan."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	newPoints, err := b.addPoints(memberID, potongan)
	if err != nil {
		content := "❌ Gagal mengurangi poin di database."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	if err := b.updateKarmaRole(s, i.GuildID, memberID, newPoints); err != nil {
		log.Printf("gagal update karma role setelah kurangi_poin: %v", err)
	}

	content := fmt.Sprintf("Berhasil memotong %d poin dari %s! Poin dia sekarang: %d.", jumlah, displayNameFromMember(member), newPoints)
	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
}

func (b *Bot) handleRemoveAccess(s *discordgo.Session, i *discordgo.InteractionCreate, options []*discordgo.ApplicationCommandInteractionDataOption) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})

	memberID := optionIDValue(optionByName(options, "member"))
	if memberID == "" {
		content := "❌ Kamu harus milih member-nya dulu kalau mau nyabut akses!"
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	b.dbMu.Lock()
	var exists int
	err := b.db.QueryRow(`SELECT 1 FROM bot_access WHERE user_id = ?`, memberID).Scan(&exists)
	if err != nil {
		b.dbMu.Unlock()
		content := "❌ Eits! Anak ini dari awal **memang tidak punya akses** magang."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	_, err = b.db.Exec(`DELETE FROM bot_access WHERE user_id = ?`, memberID)
	b.dbMu.Unlock()

	role, _ := b.roleByName(s, i.GuildID, magangRoleName)
	if role != nil {
		_ = s.GuildMemberRoleRemove(i.GuildID, memberID, role.ID)
	}

	content := fmt.Sprintf("✅ Berhasil! Hak akses database dan role **%s** dari <@%s> telah dicabut!", magangRoleName, memberID)
	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
}

func (b *Bot) handleRemoveRole(s *discordgo.Session, i *discordgo.InteractionCreate, options []*discordgo.ApplicationCommandInteractionDataOption) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})

	memberID := optionIDValue(optionByName(options, "member"))
	roleID := optionIDValue(optionByName(options, "role"))
	if memberID == "" || roleID == "" {
		content := "❌ Data member atau role tidak valid."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	member, err := s.GuildMember(i.GuildID, memberID)
	if err != nil {
		content := "❌ Member tidak ditemukan."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	role, _ := b.roleByID(s, i.GuildID, roleID)
	roleName := roleID
	if role != nil {
		roleName = role.Name
	}

	if !containsString(member.Roles, roleID) {
		content := fmt.Sprintf("Si %s emang nggak punya role %s dari awal!", displayNameFromMember(member), roleName)
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	if err := s.GuildMemberRoleRemove(i.GuildID, memberID, roleID); err != nil {
		if isForbidden(err) {
			content := "❌ Gagal! Pastikan posisi role bot ADA DI ATAS role yang mau dicabut di Server Settings!"
			_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
			return
		}
		content := "❌ Gagal mencabut role."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	content := fmt.Sprintf("✅ Berhasil mencabut role **%s** dari <@%s>!", roleName, memberID)
	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
}

func (b *Bot) handleSetupRules(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "✅ Embed Aturan Server berhasil dimunculkan!",
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("gagal respond setup_rules: %v", err)
		return
	}
	if _, err := s.ChannelMessageSendEmbed(i.ChannelID, b.getRulesEmbed()); err != nil {
		log.Printf("gagal kirim embed rules: %v", err)
	}
}

func (b *Bot) handleSetupTicket(s *discordgo.Session, i *discordgo.InteractionCreate) {
	embed := &discordgo.MessageEmbed{
		Title:       "📞 Pusat Bantuan",
		Description: "Butuh bantuan admin? Klik tombol di bawah ini untuk membuat tiket privat.",
		Color:       0x57F287,
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Panel tiket berhasil dimunculkan!",
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("gagal respond setup_tiket: %v", err)
		return
	}

	if _, err := s.ChannelMessageSendComplex(i.ChannelID, &discordgo.MessageSend{
		Embed:      embed,
		Components: ticketButtonComponents(),
	}); err != nil {
		log.Printf("gagal kirim panel tiket: %v", err)
	}
}

func (b *Bot) handleTopPoints(s *discordgo.Session, i *discordgo.InteractionCreate) {
	tampilan := optionStringValue(optionByName(i.ApplicationCommandData().Options, "tampilkan"))
	ephemeral := true
	var warningMsg string

	if tampilan == "publik" {
		if b.hasAdminAccess(s, i) {
			ephemeral = false
		} else {
			warningMsg = "❌ *Eits! Cuma Admin yang bisa pamer top poin ke publik. Punya kamu aku bikin Privat ya!*\n\n"
		}
	}

	flags := discordgo.MessageFlags(0)
	if ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: flags,
		},
	})

	b.dbMu.Lock()
	rows, err := b.db.Query(`SELECT user_id, points, msg_count FROM users ORDER BY points DESC`)
	b.dbMu.Unlock()

	if err != nil {
		content := warningMsg + "Belum ada data poin nih"
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}
	defer rows.Close()

	embed := &discordgo.MessageEmbed{
		Title: "🏆 Papan Peringkat Poin GaCor",
		Color: 0xF1C40F,
	}

	rank := 1
	for rows.Next() {
		if rank > 10 {
			break
		}

		var userID int64
		var points int64
		var msgCount int64
		if err := rows.Scan(&userID, &points, &msgCount); err != nil {
			continue
		}

		userIDStr := strconv.FormatInt(userID, 10)
		member, err := s.GuildMember(i.GuildID, userIDStr)
		if err != nil {
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
				Name:   fmt.Sprintf("Rank %d. Mantan Member", rank),
				Value:  fmt.Sprintf("🪙 Poin: %d | 💬 Chat: %d", points, msgCount),
				Inline: false,
			})
			rank++
			continue
		}

		if member.User != nil && member.User.Bot {
			continue
		}

		hasBotRole, err := b.memberHasRoleByName(s, i.GuildID, member.User.ID, botMemberRoleName)
		if err == nil && hasBotRole {
			continue
		}

		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   fmt.Sprintf("Rank %d. %s", rank, displayNameFromMember(member)),
			Value:  fmt.Sprintf("🪙 Poin: %d | 💬 Chat: %d", points, msgCount),
			Inline: false,
		})
		rank++
	}

	if rank == 1 {
		content := warningMsg + "Belum ada member manusia yang ngumpulin poin nih!"
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	if warningMsg != "" {
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &warningMsg, Embeds: &[]*discordgo.MessageEmbed{embed}})
	} else {
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Embeds: &[]*discordgo.MessageEmbed{embed}})
	}
}

func (b *Bot) handleUndian(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	go b.logMagangAction(s, i, "/undian (Publik)")

	pesertaStr := optionStringValue(optionByName(i.ApplicationCommandData().Options, "peserta"))

	re := regexp.MustCompile(`<@!?(\d+)>`)
	matches := re.FindAllStringSubmatch(pesertaStr, -1)

	if len(matches) < 2 {
		content := "❌ Minimal pesertanya 2 orang dong Chuy biar bisa diundi! (Pastikan pakai tag @nama)"
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	var winners []string
	for _, match := range matches {
		winners = append(winners, match[1])
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	r.Shuffle(len(winners), func(idx1, idx2 int) {
		winners[idx1], winners[idx2] = winners[idx2], winners[idx1]
	})

	embed := &discordgo.MessageEmbed{
		Title: "🎲 Mengocok Nama...",
		Color: 0xE67E22,
	}

	var resultText strings.Builder

	for idx, winner := range winners {
		sisaPeserta := len(winners) - idx

		if sisaPeserta > 1 {
			visualOffset := r.Intn(sisaPeserta)
			totalKedip := r.Intn(6) + 2

			for kedip := 0; kedip < totalKedip; kedip++ {
				randomVisual := winners[idx+((visualOffset+kedip)%sisaPeserta)]

				tempDesc := "Jeng jeng! Inilah urutan hasil undiannya:\n\n" + resultText.String() + fmt.Sprintf("**%d.** <@%s> 🔄\n", idx+1, randomVisual)
				embed.Description = tempDesc

				_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Embeds: &[]*discordgo.MessageEmbed{embed},
				})
				time.Sleep(500 * time.Millisecond)
			}
		}

		resultText.WriteString(fmt.Sprintf("**%d.** <@%s> ✅\n", idx+1, winner))
		embed.Description = "Jeng jeng! Inilah urutan hasil undiannya:\n\n" + resultText.String()

		if idx == len(winners)-1 {
			embed.Title = "🎉 Hasil Undian Selesai!"
			embed.Color = 0xF1C40F
		}

		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{embed},
		})

		if sisaPeserta > 1 {
			time.Sleep(800 * time.Millisecond)
		}
	}
}

func (b *Bot) handleUsirBotAntispam(s *discordgo.Session, i *discordgo.InteractionCreate) {
	respondDeferred(s, i)

	if !b.requireAdminOrAccess(s, i) {
		msg := "❌ Kamu tidak memiliki izin akses."
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
		return
	}

	botState, err := s.State.VoiceState(i.GuildID, s.State.User.ID)
	if err != nil || botState == nil || botState.ChannelID == "" {
		msg := "🤔 Aku sedang tidak berada di Voice Channel mana pun."
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
		return
	}

	err = s.ChannelVoiceJoinManual(i.GuildID, "", false, false)
	if err != nil {
		msg := fmt.Sprintf("❌ Gagal keluar dari Voice Channel: %v", err)
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
		return
	}

	msg := "👋 Bot Anti-Spam pamit! Sampai jumpa lagi."
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
}

func (b *Bot) addPoints(userID string, amount int64) (points int64, err error) {
	b.dbMu.Lock()
	defer b.dbMu.Unlock()

	id, err := strconv.ParseInt(userID, 10, 64)
	if err != nil {
		return 0, err
	}

	tx, err := b.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.Exec(`INSERT OR IGNORE INTO users (user_id, msg_count, points) VALUES (?, 0, 0)`, id); err != nil {
		return 0, err
	}

	if _, err = tx.Exec(`UPDATE users SET points = points + ? WHERE user_id = ?`, amount, id); err != nil {
		return 0, err
	}

	if err = tx.QueryRow(`SELECT points FROM users WHERE user_id = ?`, id).Scan(&points); err != nil {
		return 0, err
	}

	if err = tx.Commit(); err != nil {
		return 0, err
	}

	return points, nil
}

func (b *Bot) getRulesEmbed() *discordgo.MessageEmbed {

	garisPembatas := "\n──────────────────────────────────────\n\u200b"

	return &discordgo.MessageEmbed{
		Title:       "📜 PERATURAN RESMI SERVER GACOR",
		Description: "Harap dibaca dan dipatuhi demi kenyamanan kita bersama di tongkrongan ini!" + garisPembatas,
		Color:       0xF1C40F,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name: "📃 Aturan Utama (General Rules)\n",
				Value: "• **Hormati Sesama Member:**\n" +
					"Dilarang keras melakukan perundungan (*bullying*), pelecehan, atau ancaman kepada member lain. Gunakan bahasa yang sopan dan hargai opini orang lain.\n\n" +
					"• **Dilarang Membahas SARA & Politik:**\n" +
					"Jauhkan server ini dari perdebatan sensitif terkait Suku, Agama, Ras, dan Antargolongan, serta politik praktis yang berpotensi memicu keributan.\n\n" +
					"• **Dilarang Keras Mengirim Konten NSFW:**\n" +
					"Jangan mengirimkan gambar, video, atau tautan yang mengandung unsur pornografi, kekerasan ekstrem (*gore*), atau konten dewasa lainnya." + garisPembatas,
				Inline: false,
			},
			{
				Name: "💬 Aturan Chat & Teks\n",
				Value: "• **No Spamming:**\n" +
					"Dilarang mengirim pesan yang sama berulang-ulang, *flood text*, atau *spam mention* (seperti asal tag @everyone atau @here tanpa alasan).\n\n" +
					"• **Dilarang Promosi Sembarangan (Self-Promo):**\n" +
					"Jangan mempromosikan server Discord lain, jualan, atau link pribadi di sembarang channel. Gunakan channel khusus promosi jika sudah disediakan." + garisPembatas,
				Inline: false,
			},
			{
				Name: "🔈 Aturan Voice Channel & Mabar\n",
				Value: "• **Jaga Etika Mic (No Mic Spam):**\n" +
					"Dilarang spam soundboard serta berteriak-teriak tidak jelas. Gunakan fitur *Push to Talk* jika background ruangan kamu sedang sangat berisik.\n\n" +
					"• **Dilarang Toxic Berlebihan:**\n" +
					"Emosi saat lagi push rank atau geregetan pas mabar itu wajar, tapi jangan sampai melontarkan makian kasar yang menyerang personal ke sesama member." + garisPembatas,
				Inline: false,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Server GaCor - Main Bersih, Nongkrong Asik!",
		},
	}
}

func (b *Bot) getUserStats(userID string) (int64, int64, error) {
	b.dbMu.Lock()
	defer b.dbMu.Unlock()

	id, err := strconv.ParseInt(userID, 10, 64)
	if err != nil {
		return 0, 0, err
	}

	if _, err := b.db.Exec(`INSERT OR IGNORE INTO users (user_id, msg_count, points) VALUES (?, 0, 0)`, id); err != nil {
		return 0, 0, err
	}

	var msgCount int64
	var points int64
	if err := b.db.QueryRow(`SELECT msg_count, points FROM users WHERE user_id = ?`, id).Scan(&msgCount, &points); err != nil {
		return 0, 0, err
	}

	return msgCount, points, nil
}

func (b *Bot) hapusPengingat(s *discordgo.Session, i *discordgo.InteractionCreate, options []*discordgo.ApplicationCommandInteractionDataOption) {
	idOpt := optionByName(options, "id")
	if idOpt == nil {
		content := "❌ ID pengingat belum diisi! Cek ID-nya pakai opsi list dulu ya."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}
	targetIndex := int(idOpt.IntValue()) - 1

	b.dbMu.Lock()
	rows, err := b.db.Query(`SELECT id FROM deadlines ORDER BY waktu ASC`)
	var realIDs []int
	if err == nil {
		for rows.Next() {
			var realID int
			if rows.Scan(&realID) == nil {
				realIDs = append(realIDs, realID)
			}
		}
		rows.Close()
	}

	if targetIndex < 0 || targetIndex >= len(realIDs) {
		b.dbMu.Unlock()
		content := "❌ Nomor ID tidak ditemukan! Pastikan kamu masukin ID yang ada di list."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	realIDToDelete := realIDs[targetIndex]

	var tugas string
	var waktu time.Time
	if err := b.db.QueryRow(`SELECT tugas, waktu FROM deadlines WHERE id = ?`, realIDToDelete).Scan(&tugas, &waktu); err != nil {
		b.dbMu.Unlock()
		content := "❌ Gagal membaca data pengingat sebelum dihapus."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	_, err = b.db.Exec(`DELETE FROM deadlines WHERE id = ?`, realIDToDelete)
	b.dbMu.Unlock()

	if err != nil {
		content := "❌ Gagal menghapus pengingat dari database."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	content := fmt.Sprintf("✅ Pengingat **%s** (ID: %d) pada %s berhasil dihapus!", tugas, targetIndex+1, waktu.Format("02 January 2006 15:04"))
	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
}

func (b *Bot) hasAdminAccess(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	if i.Member == nil || i.Member.User == nil {
		return false
	}

	ok, err := b.memberHasRoleByName(s, i.GuildID, i.Member.User.ID, adminRoleName)
	if err == nil && ok {
		return true
	}

	okMagang, errMagang := b.memberHasRoleByName(s, i.GuildID, i.Member.User.ID, magangRoleName)
	if errMagang == nil && okMagang {
		return true
	}

	b.dbMu.Lock()
	defer b.dbMu.Unlock()
	var exists int
	err = b.db.QueryRow(`SELECT 1 FROM bot_access WHERE user_id = ?`, i.Member.User.ID).Scan(&exists)

	return err == nil
}

func (b *Bot) listPengingat(s *discordgo.Session, i *discordgo.InteractionCreate, options []*discordgo.ApplicationCommandInteractionDataOption) {
	tampilan := optionStringValue(optionByName(options, "tampilkan"))
	ephemeral := true
	isAdmin := b.hasAdminAccess(s, i)

	if tampilan == "publik" && isAdmin {
		ephemeral = false
	}

	flags := discordgo.MessageFlags(0)
	if ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: flags},
	})

	b.dbMu.Lock()
	rows, err := b.db.Query(`SELECT tugas, waktu, is_public FROM deadlines ORDER BY waktu ASC`)
	b.dbMu.Unlock()

	if err != nil {
		content := "❌ Gagal menarik data jadwal dari database."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}
	defer rows.Close()

	embed := &discordgo.MessageEmbed{Title: "📅 Daftar Deadline & Jadwal", Color: 0x3498DB}
	count := 0
	indexDinamis := 1

	for rows.Next() {
		var tugas string
		var waktu time.Time
		var isPublic bool
		if err := rows.Scan(&tugas, &waktu, &isPublic); err == nil {
			if !isAdmin && !isPublic {
				continue
			}

			count++
			tipe := "🎫 Privat"
			if isPublic {
				tipe = "📢 Publik"
			}
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
				Name:   fmt.Sprintf("ID: %d - %s", indexDinamis, tugas),
				Value:  fmt.Sprintf("⏰ %s | %s", waktu.Format("02 January 2006 15:04"), tipe),
				Inline: false,
			})
			indexDinamis++
		}
	}

	if count == 0 {
		content := "Belum ada jadwal publik nih. Santai dulu Choy!"
		if isAdmin {
			content = "Belum ada jadwal atau tenggat waktu sama sekali di database."
		}
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Embeds: &[]*discordgo.MessageEmbed{embed}})
}

func (b *Bot) logMagangAction(s *discordgo.Session, i *discordgo.InteractionCreate, actionName string) {
	isAdminBesar, _ := b.memberHasRoleByName(s, i.GuildID, i.Member.User.ID, adminRoleName)
	if isAdminBesar {
		return
	}

	embed := &discordgo.MessageEmbed{
		Title: "🕵️‍♂️ Log Aktivitas Admin Magang",
		Color: 0x9B59B6,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Pelaku", Value: fmt.Sprintf("<@%s>", i.Member.User.ID), Inline: true},
			{Name: "Channel", Value: fmt.Sprintf("<#%s>", i.ChannelID), Inline: true},
			{Name: "Aktivitas", Value: fmt.Sprintf("Menggunakan command: `%s`", actionName), Inline: false},
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	_, _ = s.ChannelMessageSendEmbed(channelLogAdmin, embed)
}

func (b *Bot) memberHasRoleByName(s *discordgo.Session, guildIDValue, userID, roleName string) (bool, error) {
	member, err := s.GuildMember(guildIDValue, userID)
	if err != nil {
		return false, err
	}

	role, err := b.roleByName(s, guildIDValue, roleName)
	if err != nil || role == nil {
		return false, err
	}

	return containsString(member.Roles, role.ID), nil
}

func (b *Bot) recordMessage(userID string, toxic bool) (oldPoints int64, newPoints int64, err error) {
	b.dbMu.Lock()
	defer b.dbMu.Unlock()

	id, err := strconv.ParseInt(userID, 10, 64)
	if err != nil {
		return 0, 0, err
	}

	tx, err := b.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.Exec(`INSERT OR IGNORE INTO users (user_id, msg_count, points) VALUES (?, 0, 0)`, id); err != nil {
		return 0, 0, err
	}

	// CEK POIN LAMA SEBELUM DITAMBAH
	if err = tx.QueryRow(`SELECT points FROM users WHERE user_id = ?`, id).Scan(&oldPoints); err != nil {
		return 0, 0, err
	}

	delta := int64(1)
	if toxic {
		delta = -1
	}

	if _, err = tx.Exec(`UPDATE users SET msg_count = msg_count + 1, points = points + ? WHERE user_id = ?`, delta, id); err != nil {
		return 0, 0, err
	}

	if err = tx.QueryRow(`SELECT points FROM users WHERE user_id = ?`, id).Scan(&newPoints); err != nil {
		return 0, 0, err
	}

	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}

	return oldPoints, newPoints, nil
}

func (b *Bot) requireAdminBesarOnly(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	if i.Member == nil || i.Member.User == nil {
		respondEphemeral(s, i, "❌ Waduh Chuy, data member-nya nggak kebaca!")
		return false
	}

	ok, err := b.memberHasRoleByName(s, i.GuildID, i.Member.User.ID, adminRoleName)
	if err != nil || !ok {
		respondEphemeral(s, i, "❌ DITOLAK! Perintah ini SANGAT RAHASIA dan cuma bisa diakses sama **『 👑 』ADMIN BESAR** asli!")
		return false
	}

	return true
}

func (b *Bot) requireAdminOrAccess(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	if !b.hasAdminAccess(s, i) {
		respondEphemeral(s, i, "❌ DITOLAK! Kamu bukan Admin Besar atau Admin Magang. Nggak boleh nyentuh tombol ini!")
		return false
	}
	return true
}

func (b *Bot) roleByID(s *discordgo.Session, guildIDValue, roleID string) (*discordgo.Role, error) {
	roles, err := s.GuildRoles(guildIDValue)
	if err != nil {
		return nil, err
	}

	for _, role := range roles {
		if role.ID == roleID {
			return role, nil
		}
	}

	return nil, nil
}

func (b *Bot) roleByName(s *discordgo.Session, guildIDValue, roleName string) (*discordgo.Role, error) {
	roles, err := s.GuildRoles(guildIDValue)
	if err != nil {
		return nil, err
	}

	for _, role := range roles {
		if role.Name == roleName {
			return role, nil
		}
	}

	return nil, nil
}

func (b *Bot) runPromotionScan(s *discordgo.Session) {
	roles, err := s.GuildRoles(guildID)
	if err != nil {
		return
	}

	var vetRoleID, senRoleID string
	var adminRoleID, modRoleID, helperRoleID string

	for _, r := range roles {
		switch r.Name {
		case roleTingkatan[3]:
			vetRoleID = r.ID
		case roleTingkatan[4]:
			senRoleID = r.ID
		case roleTingkatan[0]:
			adminRoleID = r.ID
		case roleTingkatan[1]:
			modRoleID = r.ID
		case roleTingkatan[2]:
			helperRoleID = r.ID
		}
	}

	guild, err := s.State.Guild(guildID)
	if err != nil {
		return
	}

	for _, member := range guild.Members {
		if member.User == nil || member.User.Bot {
			continue
		}

		if containsString(member.Roles, adminRoleID) || containsString(member.Roles, modRoleID) || containsString(member.Roles, helperRoleID) {
			continue
		}

		hariGabung := time.Since(member.JoinedAt).Hours() / 24
		isVeteran := containsString(member.Roles, vetRoleID)
		isSenior := containsString(member.Roles, senRoleID)

		if hariGabung >= 365 && !isVeteran && vetRoleID != "" {
			_ = s.GuildMemberRoleAdd(guildID, member.User.ID, vetRoleID)
			if senRoleID != "" {
				_ = s.GuildMemberRoleRemove(guildID, member.User.ID, senRoleID)
			}
			time.Sleep(500 * time.Millisecond)
		} else if hariGabung >= 90 && !isSenior && !isVeteran && senRoleID != "" {
			_ = s.GuildMemberRoleAdd(guildID, member.User.ID, senRoleID)
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func (b *Bot) sendAdminLog(s *discordgo.Session, m *discordgo.MessageCreate) error {
	embed := &discordgo.MessageEmbed{
		Title: "🚨 Laporan Perilaku Buruk",
		Color: 0xED4245,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Pelaku", Value: fmt.Sprintf("<@%s>", m.Author.ID), Inline: true},
			{Name: "Channel", Value: fmt.Sprintf("<#%s>", m.ChannelID), Inline: true},
			{Name: "Isi Pesan", Value: fmt.Sprintf("\x60\x60\x60%s\x60\x60\x60", truncate(m.Content, 950)), Inline: false},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("ID User: %s", m.Author.ID),
		},
	}

	_, err := s.ChannelMessageSendEmbed(channelLogAdmin, embed)
	return err
}

func (b *Bot) startAutoDisconnectChecker(s *discordgo.Session) {
	ticker := time.NewTicker(1 * time.Minute)
	aloneSince := time.Time{}
	isAlone := false

	go func() {
		for range ticker.C {
			guild, err := s.State.Guild(guildID)
			if err != nil {
				continue
			}

			botState, err := s.State.VoiceState(guildID, s.State.User.ID)
			if err != nil || botState == nil || botState.ChannelID == "" {
				isAlone = false
				continue
			}

			humanCount := 0
			for _, vs := range guild.VoiceStates {
				if vs.ChannelID == botState.ChannelID && vs.UserID != s.State.User.ID {
					member, err := s.State.Member(guildID, vs.UserID)
					if err != nil {
						member, err = s.GuildMember(guildID, vs.UserID)
					}

					isBot := false
					if member != nil {
						if member.User != nil && member.User.Bot {
							isBot = true
						} else {
							hasBotRole, _ := b.memberHasRoleByName(s, guildID, vs.UserID, botMemberRoleName)
							if hasBotRole {
								isBot = true
							}
						}
					}

					if !isBot {
						humanCount++
					}
				}
			}

			if humanCount == 0 {
				if !isAlone {
					isAlone = true
					aloneSince = time.Now()
				} else if time.Since(aloneSince) >= (2*time.Minute + 30*time.Second) {
					err := s.ChannelVoiceJoinManual(guildID, "", false, false)
					if err == nil {
						isAlone = false
						pesan := fmt.Sprintf("💤 Satpam ROZON otomatis keluar dari <#%s> karena tidak ada anggota member selama 2 menit 30 detik.", botState.ChannelID)
						_, _ = s.ChannelMessageSend(channelLogAdmin, pesan)
					} else {
						log.Printf("Gagal auto-disconnect bot dari VC: %v", err)
					}
				}
			} else {
				isAlone = false
			}
		}
	}()
}

func (b *Bot) startAutoPromotionChecker(s *discordgo.Session) {
	ticker := time.NewTicker(24 * time.Hour)
	go func() {
		time.Sleep(30 * time.Second)
		b.runPromotionScan(s)
		for range ticker.C {
			b.runPromotionScan(s)
		}
	}()
}

func (b *Bot) startSoundboardCleaner() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		for range ticker.C {
			sbTrackerMu.Lock()
			sekarang := time.Now()
			for userID, times := range sbTracker {
				if len(times) > 0 && sekarang.Sub(times[len(times)-1]) > sbTimeWindow {
					delete(sbTracker, userID)
				}
			}
			sbTrackerMu.Unlock()
		}
	}()
}

func (b *Bot) startDeadlineChecker(s *discordgo.Session) {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for range ticker.C {
			loc, err := time.LoadLocation("Asia/Jakarta")
			if err != nil {
				loc = time.Local
			}
			now := time.Now().In(loc)

			b.dbMu.Lock()
			rows, err := b.db.Query(`SELECT id, user_id, channel_id, tugas, is_public FROM deadlines WHERE waktu <= ?`, now)
			b.dbMu.Unlock()

			if err != nil {
				continue
			}

			var expiredIDs []string
			for rows.Next() {
				var id int
				var userID, channelID, tugas string
				var isPublic bool
				if err := rows.Scan(&id, &userID, &channelID, &tugas, &isPublic); err != nil {
					continue
				}

				expiredIDs = append(expiredIDs, strconv.Itoa(id))

				if isPublic {
					_, _ = s.ChannelMessageSend(channelPengumuman, fmt.Sprintf("📢 **PENGUMUMAN JADWAL!**\n@everyone **%s** sudah tiba chuyy! (Pembuat: <@%s>)", tugas, userID))
				} else {
					embed := &discordgo.MessageEmbed{
						Title: "⏰ Pengingat Privat (Selesai)",
						Color: 0x3498DB,
						Fields: []*discordgo.MessageEmbedField{
							{
								Name:   "Jadwal:",
								Value:  fmt.Sprintf("```\n%s\n```", tugas),
								Inline: false,
							},
							{
								Name:   "Tanggal:",
								Value:  now.Format("02 January 2006"),
								Inline: true,
							},
							{
								Name:   "Pemilik:",
								Value:  fmt.Sprintf("<@%s>", userID),
								Inline: true,
							},
						},
						Footer: &discordgo.MessageEmbedFooter{
							Text: "Status: Selesai Tepat Waktu",
						},
						Timestamp: now.Format(time.RFC3339),
					}
					_, _ = s.ChannelMessageSendEmbed(channelLogAdmin, embed)
				}
			}
			rows.Close()

			if len(expiredIDs) > 0 {
				b.dbMu.Lock()
				_, _ = b.db.Exec(fmt.Sprintf(`DELETE FROM deadlines WHERE id IN (%s)`, strings.Join(expiredIDs, ",")))
				b.dbMu.Unlock()
			}
		}
	}()
}

func (b *Bot) tambahPengingat(s *discordgo.Session, i *discordgo.InteractionCreate, options []*discordgo.ApplicationCommandInteractionDataOption) {
	tugas := optionStringValue(optionByName(options, "tugas"))
	waktuStr := optionStringValue(optionByName(options, "waktu"))

	if tugas == "" || waktuStr == "" {
		content := "❌ Nama tugas dan waktunya harus diisi!"
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	tampilan := optionStringValue(optionByName(options, "tampilkan"))
	isPublic := (tampilan == "publik")

	loc, errLoc := time.LoadLocation("Asia/Jakarta")
	if errLoc != nil {
		loc = time.Local
	}

	waktu, err := time.ParseInLocation("02-01-2006 15:04", waktuStr, loc)
	if err != nil {
		content := "❌ Format waktu salah! Gunakan: `DD-MM-YYYY HH:MM`"
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	if waktu.Before(time.Now()) {
		name := "Zon"
		if i.Member != nil {
			name = displayNameFromInteraction(i)
		}
		content := fmt.Sprintf("❌ Waktunya udah lewat, %s! Ngapain ditambah?", name)
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	b.dbMu.Lock()
	_, err = b.db.Exec(`INSERT INTO deadlines (user_id, channel_id, tugas, waktu, is_public) VALUES (?, ?, ?, ?, ?)`,
		i.Member.User.ID, i.ChannelID, tugas, waktu, isPublic)
	b.dbMu.Unlock()

	if err != nil {
		content := "❌ Gagal menyimpan ke database."
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
		return
	}

	tipe := "Privat"
	if isPublic {
		tipe = "Publik"
	}
	formattedWaktu := waktu.Format("02 January 2006 15:04")
	content := fmt.Sprintf("✅ Mantap! Pengingat **%s** berhasil disimpan untuk %s.\nMode: %s", tugas, formattedWaktu, tipe)
	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
}

func (b *Bot) updateKarmaRole(s *discordgo.Session, guildIDValue, userID string, points int64) error {
	member, err := s.GuildMember(guildIDValue, userID)
	if err != nil {
		return err
	}

	targetRoleName := karmaRoleNameForPoints(points)
	targetRole, err := b.roleByName(s, guildIDValue, targetRoleName)
	if err != nil || targetRole == nil {
		return err
	}

	if containsString(member.Roles, targetRole.ID) {
		return nil
	}

	for _, roleName := range karmaRoleNames {
		role, err := b.roleByName(s, guildIDValue, roleName)
		if err != nil || role == nil || role.ID == targetRole.ID {
			continue
		}

		if containsString(member.Roles, role.ID) {
			_ = s.GuildMemberRoleRemove(guildIDValue, userID, role.ID)
		}
	}

	return s.GuildMemberRoleAdd(guildIDValue, userID, targetRole.ID)
}

func closeTicketButtonComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "Tutup Tiket", Style: discordgo.DangerButton, Emoji: &discordgo.ComponentEmoji{Name: "🔒"}, CustomID: "tutup_tiket"},
			},
		},
	}
}

func containsAny(content string, _ []string) bool {
	return badWordRegex.MatchString(content)
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func displayNameFromInteraction(i *discordgo.InteractionCreate) string {
	if i.Member != nil {
		return displayNameFromMember(i.Member)
	}
	if i.User != nil {
		if i.User.GlobalName != "" {
			return i.User.GlobalName
		}
		return i.User.Username
	}
	return "member"
}

func displayNameFromMember(member *discordgo.Member) string {
	if member == nil {
		return "Member"
	}
	if member.Nick != "" {
		return member.Nick
	}
	if member.User != nil {
		if member.User.GlobalName != "" {
			return member.User.GlobalName
		}
		if member.User.Username != "" {
			return member.User.Username
		}
	}
	return "Member"
}

func displayNameFromMessage(m *discordgo.MessageCreate) string {
	if m.Member != nil && m.Member.Nick != "" {
		return m.Member.Nick
	}
	if m.Author != nil {
		if m.Author.GlobalName != "" {
			return m.Author.GlobalName
		}
		if m.Author.Username != "" {
			return m.Author.Username
		}
	}
	return "Member"
}

func isForbidden(err error) bool {
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Message != nil {
		return restErr.Message.Code == discordgo.ErrCodeMissingPermissions
	}
	return false
}

func karmaRoleNameForPoints(points int64) string {
	switch {
	case points >= 100:
		return "「 😇 」sangat baik"
	case points >= 30:
		return "「 ☺️ 」orang baik"
	case points >= 0:
		return "「 😐 」netral"
	case points >= -50:
		return "「 😠 」tidak baik"
	default:
		return "「 😈 」orang jahat"
	}
}

func optionByName(options []*discordgo.ApplicationCommandInteractionDataOption, name string) *discordgo.ApplicationCommandInteractionDataOption {
	for _, option := range options {
		if option.Name == name {
			return option
		}
	}
	return nil
}

func optionIDValue(option *discordgo.ApplicationCommandInteractionDataOption) string {
	if option == nil {
		return ""
	}

	switch value := option.Value.(type) {
	case string:
		return value
	case float64:
		return strconv.FormatInt(int64(value), 10)
	default:
		return fmt.Sprint(value)
	}
}

func optionStringValue(option *discordgo.ApplicationCommandInteractionDataOption) string {
	if option == nil {
		return ""
	}
	return option.StringValue()
}

func respond(s *discordgo.Session, i *discordgo.InteractionCreate, content string, ephemeral bool) {
	flags := discordgo.MessageFlags(0)
	if ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   flags,
		},
	}); err != nil {
		log.Printf("gagal merespons interaction: %v", err)
	}
}

func respondDeferred(s *discordgo.Session, i *discordgo.InteractionCreate) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
}

func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	respond(s, i, content, true)
}

func respondWithEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed, ephemeral bool) {
	flags := discordgo.MessageFlags(0)
	if ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
			Flags:  flags,
		},
	}); err != nil {
		log.Printf("gagal merespons embed interaction: %v", err)
	}
}

func sanitizeTextChannelName(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	replacer := strings.NewReplacer(" ", "-", "_", "-", ".", "-", ",", "-", "--", "-")
	text = replacer.Replace(text)

	var builder strings.Builder
	lastDash := false

	for _, r := range text {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			lastDash = false
			continue
		}

		if r == '-' && !lastDash {
			builder.WriteRune('-')
			lastDash = true
		}
	}

	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "member"
	}
	return result
}

func ticketButtonComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "Buat Tiket Bantuan", Style: discordgo.SuccessButton, Emoji: &discordgo.ComponentEmoji{Name: "🎫"}, CustomID: "tiket_bantuan"},
			},
		},
	}
}

func truncate(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max-3] + "..."
}

package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"
)

// Version number
const VERSION = "1.1.7"

// Rate-limiting configuration
const MESSAGE_LIMIT = 5
const TIME_WINDOW = 1 * time.Second // Time window

var (
	// in-memory storage
	channelStates = struct {
		sync.RWMutex
		m map[int64]bool
	}{m: make(map[int64]bool)}

	botSettings = struct {
		sync.RWMutex
		m map[int64]*GuildSettings
	}{m: make(map[int64]*GuildSettings)}

	// rate limiter
	tsMutex sync.Mutex
	times   []time.Time

	// Discord owner (single user allowed to run owner command)
	ownerID string

	statuses = []string{
		"for Twitter links", "for Reddit links", "for Instagram links", "for Threads links", "for Pixiv links", "for Bluesky links",
	}
)

// GuildSettings mirrors the Python structure
type GuildSettings struct {
	EnabledServices []string
	MentionUsers    bool
	DeleteOriginal  bool
}

func defaultServices() []string {
	return []string{"Twitter", "Instagram", "Reddit", "Threads", "Pixiv", "Bluesky"}
}

func rateLimitedSend(s *discordgo.Session, channelID string, content string) error {
	// Simple sliding-window rate limiter matching Python behaviour
	for {
		tsMutex.Lock()
		now := time.Now()
		// remove old timestamps
		clean := 0
		for i, t := range times {
			if now.Sub(t) >= TIME_WINDOW {
				clean = i + 1
			} else {
				break
			}
		}
		if clean > 0 {
			times = times[clean:]
		}
		if len(times) < MESSAGE_LIMIT {
			times = append(times, now)
			tsMutex.Unlock()
			_, err := s.ChannelMessageSend(channelID, content)
			return err
		}
		tsMutex.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
}

func createFooter(embed *discordgo.MessageEmbed, s *discordgo.Session) {
	if s.State != nil && s.State.User != nil {
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text:    fmt.Sprintf("%s | v%s", s.State.User.Username, VERSION),
			IconURL: s.State.User.AvatarURL(""),
		}
	}
}

func initDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// Make sure the database is responsive
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS channel_states (channel_id INTEGER PRIMARY KEY, state BOOLEAN)`)
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS guild_settings (guild_id INTEGER PRIMARY KEY, enabled_services TEXT, mention_users BOOLEAN, delete_original BOOLEAN DEFAULT 1)`)
	if err != nil {
		return nil, err
	}

	// Add columns if missing (sqlite will error - ignore duplicate column)
	_, _ = db.Exec(`ALTER TABLE guild_settings ADD COLUMN mention_users BOOLEAN DEFAULT 1`)
	_, _ = db.Exec(`ALTER TABLE guild_settings ADD COLUMN delete_original BOOLEAN DEFAULT 1`)

	return db, nil
}

func loadChannelStates(db *sql.DB, dg *discordgo.Session) error {
	rows, err := db.Query("SELECT channel_id, state FROM channel_states")
	if err != nil {
		return err
	}
	defer rows.Close()
	channelStates.Lock()
	for rows.Next() {
		var channelID int64
		var state int
		if err := rows.Scan(&channelID, &state); err != nil {
			continue
		}
		channelStates.m[channelID] = state != 0
	}
	channelStates.Unlock()

	// Ensure any channels we haven't seen default to true.
	// Use current guilds in session state (populated after ready)
	if dg != nil && dg.State != nil {
		channelStates.Lock()
		for _, g := range dg.State.Guilds {
			for _, ch := range g.Channels {
				// only text channels (type 0)
				if ch.Type == discordgo.ChannelTypeGuildText {
					cidInt, _ := discordIDStringToInt64(ch.ID)
					if _, ok := channelStates.m[cidInt]; !ok {
						channelStates.m[cidInt] = true
					}
				}
			}
		}
		channelStates.Unlock()
	}

	return nil
}

func loadSettings(db *sql.DB) error {
	rows, err := db.Query("SELECT guild_id, enabled_services, mention_users, delete_original FROM guild_settings")
	if err != nil {
		return err
	}
	defer rows.Close()

	botSettings.Lock()
	defer botSettings.Unlock()

	for rows.Next() {
		var guildID int64
		var enabledServices sql.NullString
		var mentionUsers sql.NullBool
		var deleteOriginal sql.NullBool
		if err := rows.Scan(&guildID, &enabledServices, &mentionUsers, &deleteOriginal); err != nil {
			continue
		}
		var svcList []string
		if enabledServices.Valid && enabledServices.String != "" {
			// stored as Python repr(list) in original; attempt simple parse: remove brackets and quotes
			s := enabledServices.String
			s = strings.TrimSpace(s)
			s = strings.TrimPrefix(s, "[")
			s = strings.TrimSuffix(s, "]")
			parts := strings.Split(s, ",")
			for _, p := range parts {
				q := strings.TrimSpace(p)
				q = strings.Trim(q, `"'`)
				if q != "" {
					svcList = append(svcList, q)
				}
			}
		}
		if len(svcList) == 0 {
			svcList = defaultServices()
		}
		mention := true
		if mentionUsers.Valid {
			mention = mentionUsers.Bool
		}
		deleteO := true
		if deleteOriginal.Valid {
			deleteO = deleteOriginal.Bool
		}
		botSettings.m[guildID] = &GuildSettings{
			EnabledServices: svcList,
			MentionUsers:    mention,
			DeleteOriginal:  deleteO,
		}
	}

	return nil
}
func getGuildSettingsFromDB(db *sql.DB, guildID int64) (*GuildSettings, error) {
	// Try to read a single guild's settings from DB and parse them into GuildSettings.
	row := db.QueryRow("SELECT enabled_services, mention_users, delete_original FROM guild_settings WHERE guild_id = ?", guildID)
	var enabledServices sql.NullString
	var mentionUsers sql.NullBool
	var deleteOriginal sql.NullBool
	if err := row.Scan(&enabledServices, &mentionUsers, &deleteOriginal); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	var svcList []string
	if enabledServices.Valid && enabledServices.String != "" {
		s := enabledServices.String
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "[")
		s = strings.TrimSuffix(s, "]")
		parts := strings.Split(s, ",")
		for _, p := range parts {
			q := strings.TrimSpace(p)
			q = strings.Trim(q, `"'`)
			if q != "" {
				svcList = append(svcList, q)
			}
		}
	}
	if len(svcList) == 0 {
		svcList = defaultServices()
	}

	mention := true
	if mentionUsers.Valid {
		mention = mentionUsers.Bool
	}
	deleteO := true
	if deleteOriginal.Valid {
		deleteO = deleteOriginal.Bool
	}

	return &GuildSettings{
		EnabledServices: svcList,
		MentionUsers:    mention,
		DeleteOriginal:  deleteO,
	}, nil
}

func updateChannelState(db *sql.DB, channelID int64, state bool) error {
	// retry on locked
	var lastErr error
	for i := 0; i < 5; i++ {
		_, err := db.Exec("INSERT OR REPLACE INTO channel_states (channel_id, state) VALUES (?, ?)", channelID, boolToInt(state))
		if err == nil {
			return nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "database is locked") {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return err
	}
	return lastErr
}

func updateSetting(db *sql.DB, guildID int64, enabledServices []string, mentionUsers bool, deleteOriginal bool) error {
	// store enabledServices as a simple CSV-ish Python-like repr: ['A','B']
	// We'll store as "['A','B']" to remain close to Python repr used previously.
	parts := make([]string, 0, len(enabledServices))
	for _, s := range enabledServices {
		parts = append(parts, fmt.Sprintf("%q", s))
	}
	stored := "[" + strings.Join(parts, ", ") + "]"

	var lastErr error
	for i := 0; i < 5; i++ {
		_, err := db.Exec("INSERT OR REPLACE INTO guild_settings (guild_id, enabled_services, mention_users, delete_original) VALUES (?, ?, ?, ?)",
			guildID, stored, mentionUsers, deleteOriginal)
		if err == nil {
			return nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "database is locked") {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return err
	}
	return lastErr
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Interaction (slash command) handling
func onInteractionCreate(db *sql.DB, s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Only handle application commands and component interactions
	if i.Type == discordgo.InteractionApplicationCommand {
		switch i.ApplicationCommandData().Name {
		case "activate":
			// optional channel option
			var channelID string
			opts := i.ApplicationCommandData().Options
			if len(opts) > 0 && opts[0].Type == discordgo.ApplicationCommandOptionChannel {
				channelID = opts[0].Value.(string)
			} else {
				channelID = i.ChannelID
			}
			// mark as active
			cidInt, _ := discordIDStringToInt64(channelID)
			channelStates.Lock()
			channelStates.m[cidInt] = true
			channelStates.Unlock()
			_ = updateChannelState(db, cidInt, true)

			embed := &discordgo.MessageEmbed{
				Title:       s.State.User.Username,
				Description: fmt.Sprintf("✅ Activated for <#%s>!", channelID),
				Color:       0x78b159,
			}
			createFooter(embed, s)
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Embeds: []*discordgo.MessageEmbed{embed},
				},
			})
		case "deactivate":
			var channelID string
			opts := i.ApplicationCommandData().Options
			if len(opts) > 0 && opts[0].Type == discordgo.ApplicationCommandOptionChannel {
				channelID = opts[0].Value.(string)
			} else {
				channelID = i.ChannelID
			}
			cidInt, _ := discordIDStringToInt64(channelID)
			channelStates.Lock()
			channelStates.m[cidInt] = false
			channelStates.Unlock()
			_ = updateChannelState(db, cidInt, false)

			embed := &discordgo.MessageEmbed{
				Title:       s.State.User.Username,
				Description: fmt.Sprintf("❌ Deactivated for <#%s>!", channelID),
				Color:       0xff0000, // red
			}
			createFooter(embed, s)
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Embeds: []*discordgo.MessageEmbed{embed},
				},
			})
		case "about":
			embed := &discordgo.MessageEmbed{
				Title:       "About",
				Description: "This bot fixes the lack of embed support in Discord.",
				Color:       0x7289DA,
			}
			embed.Fields = []*discordgo.MessageEmbedField{
				{
					Name: "🎉 Quick Links",
					Value: "- [Invite FixEmbed](https://discord.com/oauth2/authorize?client_id=1360722454678605914)\n" +
						"- [Star our Source Code on GitHub](https://github.com/ld3z/fixembed-go)",
					Inline: false,
				},
				{
					Name: "📜 Credits",
					Value: "- [FxTwitter](https://github.com/FixTweet/FxTwitter), created by FixTweet\n" +
						"- [InstaFix](https://github.com/Wikidepia/InstaFix), created by Wikidepia\n" +
						"- [vxReddit](https://github.com/dylanpdx/vxReddit), created by dylanpdx\n" +
						"- [fixthreads](https://github.com/milanmdev/fixthreads), created by milanmdev\n" +
						"- [phixiv](https://github.com/thelaao/phixiv), created by thelaao\n" +
						"- [VixBluesky](https://github.com/Rapougnac/VixBluesky), created by Rapougnac",
					Inline: false,
				},
			}
			createFooter(embed, s)
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Embeds: []*discordgo.MessageEmbed{embed},
				},
			})
		case "owner":
			// Owner-only command: list guilds the bot is in
			userID := ""
			if i.Member != nil && i.Member.User != nil {
				userID = i.Member.User.ID
			} else if i.User != nil {
				userID = i.User.ID
			}
			if ownerID == "" || userID != ownerID {
				// Not authorized
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "You are not authorized to use this command.",
						Flags:   1 << 6, // ephemeral
					},
				})
			} else {
				// Build guild list
				var b strings.Builder
				for _, g := range s.State.Guilds {
					b.WriteString(fmt.Sprintf("%s (ID: %s)\n", g.Name, g.ID))
				}
				if b.Len() == 0 {
					b.WriteString("Bot is not in any guilds.")
				}
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: b.String(),
						Flags:   1 << 6, // ephemeral
					},
				})
			}
		case "settings":
			// Provide a simple text-based settings reply summarizing current settings.
			guildID := i.GuildID
			var settings *GuildSettings
			if guildID == "" {
				settings = &GuildSettings{EnabledServices: defaultServices(), MentionUsers: true, DeleteOriginal: true}
			} else {
				gidInt, _ := discordIDStringToInt64(guildID)
				// First try the in-memory cache
				botSettings.RLock()
				settings = botSettings.m[gidInt]
				botSettings.RUnlock()
				// If not present, fallback to reading directly from DB (handles races / missed loads)
				if settings == nil {
					if gs, err := getGuildSettingsFromDB(db, gidInt); err == nil && gs != nil {
						settings = gs
					} else {
						settings = &GuildSettings{EnabledServices: defaultServices(), MentionUsers: true, DeleteOriginal: true}
					}
				}
			}
			serviceStatus := ""
			for _, sname := range defaultServices() {
				status := "🔴"
				for _, enabled := range settings.EnabledServices {
					if enabled == sname {
						status = "🟢"
						break
					}
				}
				serviceStatus += fmt.Sprintf("%s %s\n", status, sname)
			}
			embed := &discordgo.MessageEmbed{
				Title:       "Settings",
				Description: "Configure FixEmbed's settings",
				Color:       0x5865F2, // blurple-like
				Fields: []*discordgo.MessageEmbedField{
					{
						Name:  "Service Settings",
						Value: serviceStatus,
					},
					{
						Name:  "Mention Users",
						Value: fmt.Sprintf("%t", settings.MentionUsers),
					},
					{
						Name:  "Delete Original",
						Value: fmt.Sprintf("%t", settings.DeleteOriginal),
					},
				},
			}
			createFooter(embed, s)

			// Build the interactive settings select (mirrors Python SettingsDropdown)
			// Compute current state indicators for emojis
			activated := true
			if i.GuildID != "" && s.State != nil {
				for _, g := range s.State.Guilds {
					if g.ID == i.GuildID {
						for _, ch := range g.Channels {
							if ch.Type == discordgo.ChannelTypeGuildText {
								cidInt, _ := discordIDStringToInt64(ch.ID)
								channelStates.RLock()
								v, ok := channelStates.m[cidInt]
								channelStates.RUnlock()
								if !ok || !v {
									activated = false
									break
								}
							}
						}
						break
					}
				}
			}
			mentionUsersVal := settings.MentionUsers
			deleteOriginalVal := settings.DeleteOriginal

			settingsOptions := []discordgo.SelectMenuOption{
				{Label: "FixEmbed", Value: "FixEmbed", Description: "Activate or deactivate the bot in all channels", Emoji: &discordgo.ComponentEmoji{Name: func() string {
					if activated {
						return "🟢"
					}
					return "🔴"
				}()}},
				{Label: "Mention Users", Value: "Mention Users", Description: "Toggle mentioning users in messages", Emoji: &discordgo.ComponentEmoji{Name: func() string {
					if mentionUsersVal {
						return "🔔"
					}
					return "🔕"
				}()}},
				{Label: "Delivery Method", Value: "Delivery Method", Description: "Toggle original message deletion", Emoji: &discordgo.ComponentEmoji{Name: func() string {
					if deleteOriginalVal {
						return "📬"
					}
					return "📪"
				}()}},
				{Label: "Service Settings", Value: "Service Settings", Description: "Configure which services are activated", Emoji: &discordgo.ComponentEmoji{Name: "⚙️"}},
				{Label: "Debug", Value: "Debug", Description: "Show current debug information", Emoji: &discordgo.ComponentEmoji{Name: "🐞"}},
			}
			minVal := new(int)
			*minVal = 1
			maxVal := 1
			settingsSM := &discordgo.SelectMenu{
				CustomID:    "settings_select",
				Placeholder: "Choose an option...",
				MinValues:   minVal,
				MaxValues:   maxVal,
				Options:     settingsOptions,
			}
			components := []discordgo.MessageComponent{
				&discordgo.ActionsRow{Components: []discordgo.MessageComponent{settingsSM}},
			}

			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Embeds:     []*discordgo.MessageEmbed{embed},
					Components: components,
					Flags:      1 << 6, // ephemeral
				},
			})
		}
	}
	// Handle component interactions (select menus / buttons) similar to the Python version
	if i.Type == discordgo.InteractionMessageComponent {
		data := i.MessageComponentData()
		custom := data.CustomID
		guildID := i.GuildID
		var gidInt int64
		if guildID != "" {
			gidInt, _ = discordIDStringToInt64(guildID)
		}

		switch custom {
		case "settings_select":
			choice := ""
			if len(data.Values) > 0 {
				choice = data.Values[0]
			}
			switch choice {
			case "Service Settings":
				// Build services multi-select reflecting current settings
				var current []string
				if gidInt != 0 {
					botSettings.RLock()
					if gs, ok := botSettings.m[gidInt]; ok && gs != nil {
						current = gs.EnabledServices
					}
					botSettings.RUnlock()
					if current == nil {
						if gs2, _ := getGuildSettingsFromDB(db, gidInt); gs2 != nil {
							current = gs2.EnabledServices
						}
					}
				}
				if current == nil {
					current = defaultServices()
				}
				opts := make([]discordgo.SelectMenuOption, 0, len(defaultServices()))
				for _, svc := range defaultServices() {
					def := false
					for _, en := range current {
						if en == svc {
							def = true
							break
						}
					}
					opts = append(opts, discordgo.SelectMenuOption{Label: svc, Value: svc, Default: def})
				}
				// Provide pointer values for MinValues which this discordgo expects, and a plain int for MaxValues.
				minVal := new(int)
				*minVal = 1
				maxVal := len(opts)
				sm := &discordgo.SelectMenu{
					CustomID:    "service_select",
					Placeholder: "Select services to activate...",
					MinValues:   minVal,
					MaxValues:   maxVal,
					Options:     opts,
				}
				components := []discordgo.MessageComponent{
					&discordgo.ActionsRow{Components: []discordgo.MessageComponent{sm}},
				}
				embed := &discordgo.MessageEmbed{Title: "Service Settings", Description: "Configure which services are activated.", Color: 0x5865F2}
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseUpdateMessage,
					Data: &discordgo.InteractionResponseData{
						Embeds:     []*discordgo.MessageEmbed{embed},
						Components: components,
					},
				})
			case "Mention Users":
				// Build a toggle button that reflects current state
				mentionVal := true
				if gidInt != 0 {
					if gs, ok := botSettings.m[gidInt]; ok && gs != nil {
						mentionVal = gs.MentionUsers
					} else if gs2, _ := getGuildSettingsFromDB(db, gidInt); gs2 != nil {
						mentionVal = gs2.MentionUsers
					}
				}
				label := "Activated"
				style := discordgo.SuccessButton
				if !mentionVal {
					label = "Deactivated"
					style = discordgo.DangerButton
				}
				btn := &discordgo.Button{
					CustomID: "toggle_mention",
					Label:    label,
					Style:    style,
				}
				components := []discordgo.MessageComponent{
					&discordgo.ActionsRow{Components: []discordgo.MessageComponent{btn}},
				}
				embed := &discordgo.MessageEmbed{Title: "Mention Users Settings", Description: "Toggle mentioning users in messages.", Color: 0x00ff00}
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseUpdateMessage,
					Data: &discordgo.InteractionResponseData{
						Embeds:     []*discordgo.MessageEmbed{embed},
						Components: components,
					},
				})
			case "Delivery Method":
				// Build a toggle button reflecting current delete_original state
				deleteVal := true
				if gidInt != 0 {
					if gs, ok := botSettings.m[gidInt]; ok && gs != nil {
						deleteVal = gs.DeleteOriginal
					} else if gs2, _ := getGuildSettingsFromDB(db, gidInt); gs2 != nil {
						deleteVal = gs2.DeleteOriginal
					}
				}
				label := "Activated"
				style := discordgo.SuccessButton
				if !deleteVal {
					label = "Deactivated"
					style = discordgo.DangerButton
				}
				btn := &discordgo.Button{
					CustomID: "toggle_delete",
					Label:    label,
					Style:    style,
				}
				components := []discordgo.MessageComponent{
					&discordgo.ActionsRow{Components: []discordgo.MessageComponent{btn}},
				}
				embed := &discordgo.MessageEmbed{Title: "Delivery Method Settings", Description: "Toggle original message deletion.", Color: 0x00ff00}
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseUpdateMessage,
					Data: &discordgo.InteractionResponseData{
						Embeds:     []*discordgo.MessageEmbed{embed},
						Components: components,
					},
				})
			case "FixEmbed":
				// Build a toggle button that reflects whether all guild channels are activated
				activated := true
				if gidInt != 0 && s.State != nil {
					for _, g := range s.State.Guilds {
						if g.ID == guildID {
							for _, ch := range g.Channels {
								if ch.Type == discordgo.ChannelTypeGuildText {
									cidInt, _ := discordIDStringToInt64(ch.ID)
									channelStates.RLock()
									v, ok := channelStates.m[cidInt]
									channelStates.RUnlock()
									if !ok || !v {
										activated = false
										break
									}
								}
							}
							break
						}
					}
				}
				label := "Activated"
				style := discordgo.SuccessButton
				if !activated {
					label = "Deactivated"
					style = discordgo.DangerButton
				}
				btn := &discordgo.Button{
					CustomID: "toggle_fixembed",
					Label:    label,
					Style:    style,
				}
				components := []discordgo.MessageComponent{
					&discordgo.ActionsRow{Components: []discordgo.MessageComponent{btn}},
				}
				embed := &discordgo.MessageEmbed{Title: "FixEmbed Settings", Description: "Activate/Deactivate FixEmbed across channels.", Color: 0x00ff00}
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseUpdateMessage,
					Data: &discordgo.InteractionResponseData{
						Embeds:     []*discordgo.MessageEmbed{embed},
						Components: components,
					},
				})
			case "Debug":
				embed := &discordgo.MessageEmbed{Title: "Debug Info", Description: "Debug information (opened via components).", Color: 0x7289DA}
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Embeds: []*discordgo.MessageEmbed{embed},
						Flags:  1 << 6, // ephemeral
					},
				})
			}
		case "service_select":
			values := data.Values
			// Persist selection and update in-memory settings
			if gidInt != 0 {
				gs, _ := getGuildSettingsFromDB(db, gidInt)
				mention := true
				deleteO := true
				if gs != nil {
					mention = gs.MentionUsers
					deleteO = gs.DeleteOriginal
				}
				_ = updateSetting(db, gidInt, values, mention, deleteO)
				botSettings.Lock()
				botSettings.m[gidInt] = &GuildSettings{EnabledServices: values, MentionUsers: mention, DeleteOriginal: deleteO}
				botSettings.Unlock()
			}

			// Rebuild the services multi-select with current selection set as defaults
			enabled := values
			if len(enabled) == 0 {
				enabled = defaultServices()
			}
			opts := make([]discordgo.SelectMenuOption, 0, len(defaultServices()))
			for _, svc := range defaultServices() {
				def := false
				for _, en := range enabled {
					if en == svc {
						def = true
						break
					}
				}
				opts = append(opts, discordgo.SelectMenuOption{Label: svc, Value: svc, Default: def})
			}
			minVal := new(int)
			*minVal = 1
			maxVal := len(opts)
			serviceSM := &discordgo.SelectMenu{
				CustomID:    "service_select",
				Placeholder: "Select services to activate...",
				MinValues:   minVal,
				MaxValues:   maxVal,
				Options:     opts,
			}

			// Rebuild the settings select so both appear together (mirrors Python view)
			settingsOptions := []discordgo.SelectMenuOption{
				{Label: "FixEmbed", Value: "FixEmbed", Description: "Activate or deactivate the bot in all channels"},
				{Label: "Mention Users", Value: "Mention Users", Description: "Toggle mentioning users in messages"},
				{Label: "Delivery Method", Value: "Delivery Method", Description: "Toggle original message deletion"},
				{Label: "Service Settings", Value: "Service Settings", Description: "Configure which services are activated"},
				{Label: "Debug", Value: "Debug", Description: "Show current debug information"},
			}
			setMin := new(int)
			*setMin = 1
			setMax := 1
			settingsSM := &discordgo.SelectMenu{
				CustomID:    "settings_select",
				Placeholder: "Choose an option...",
				MinValues:   setMin,
				MaxValues:   setMax,
				Options:     settingsOptions,
			}

			components := []discordgo.MessageComponent{
				&discordgo.ActionsRow{Components: []discordgo.MessageComponent{serviceSM}},
				&discordgo.ActionsRow{Components: []discordgo.MessageComponent{settingsSM}},
			}

			embed := &discordgo.MessageEmbed{Title: "Service Settings", Description: "Saved service settings.", Color: 0x5865F2}
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseUpdateMessage,
				Data: &discordgo.InteractionResponseData{
					Embeds:     []*discordgo.MessageEmbed{embed},
					Components: components,
				},
			})
		case "toggle_mention":
			if gidInt != 0 {
				gs, _ := getGuildSettingsFromDB(db, gidInt)
				mention := true
				services := defaultServices()
				deleteO := true
				if gs != nil {
					mention = gs.MentionUsers
					services = gs.EnabledServices
					deleteO = gs.DeleteOriginal
				}
				mention = !mention
				_ = updateSetting(db, gidInt, services, mention, deleteO)
				botSettings.Lock()
				botSettings.m[gidInt] = &GuildSettings{EnabledServices: services, MentionUsers: mention, DeleteOriginal: deleteO}
				botSettings.Unlock()

				// Build updated toggle button reflecting new state
				label := "Activated"
				style := discordgo.SuccessButton
				if !mention {
					label = "Deactivated"
					style = discordgo.DangerButton
				}
				btn := &discordgo.Button{
					CustomID: "toggle_mention",
					Label:    label,
					Style:    style,
				}
				components := []discordgo.MessageComponent{
					&discordgo.ActionsRow{Components: []discordgo.MessageComponent{btn}},
				}
				embed := &discordgo.MessageEmbed{Title: "Mention Users Settings", Description: "Toggled mention users.", Color: 0x00ff00}
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseUpdateMessage,
					Data: &discordgo.InteractionResponseData{
						Embeds:     []*discordgo.MessageEmbed{embed},
						Components: components,
					},
				})
			} else {
				embed := &discordgo.MessageEmbed{Title: "Mention Users Settings", Description: "Toggled mention users.", Color: 0x00ff00}
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseUpdateMessage,
					Data: &discordgo.InteractionResponseData{
						Embeds: []*discordgo.MessageEmbed{embed},
					},
				})
			}
		case "toggle_delete":
			if gidInt != 0 {
				gs, _ := getGuildSettingsFromDB(db, gidInt)
				deleteO := true
				services := defaultServices()
				mention := true
				if gs != nil {
					deleteO = gs.DeleteOriginal
					services = gs.EnabledServices
					mention = gs.MentionUsers
				}
				deleteO = !deleteO
				_ = updateSetting(db, gidInt, services, mention, deleteO)
				botSettings.Lock()
				botSettings.m[gidInt] = &GuildSettings{EnabledServices: services, MentionUsers: mention, DeleteOriginal: deleteO}
				botSettings.Unlock()

				// Build updated toggle button reflecting new state
				label := "Activated"
				style := discordgo.SuccessButton
				if !deleteO {
					label = "Deactivated"
					style = discordgo.DangerButton
				}
				btn := &discordgo.Button{
					CustomID: "toggle_delete",
					Label:    label,
					Style:    style,
				}
				components := []discordgo.MessageComponent{
					&discordgo.ActionsRow{Components: []discordgo.MessageComponent{btn}},
				}
				embed := &discordgo.MessageEmbed{Title: "Delivery Method Settings", Description: "Toggled original message deletion.", Color: 0x00ff00}
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseUpdateMessage,
					Data: &discordgo.InteractionResponseData{
						Embeds:     []*discordgo.MessageEmbed{embed},
						Components: components,
					},
				})
			} else {
				embed := &discordgo.MessageEmbed{Title: "Delivery Method Settings", Description: "Toggled original message deletion.", Color: 0x00ff00}
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseUpdateMessage,
					Data: &discordgo.InteractionResponseData{
						Embeds: []*discordgo.MessageEmbed{embed},
					},
				})
			}
		case "toggle_fixembed":
			if gidInt != 0 && s.State != nil {
				for _, g := range s.State.Guilds {
					if g.ID == guildID {
						allActivated := true
						for _, ch := range g.Channels {
							if ch.Type == discordgo.ChannelTypeGuildText {
								cidInt, _ := discordIDStringToInt64(ch.ID)
								channelStates.RLock()
								v, ok := channelStates.m[cidInt]
								channelStates.RUnlock()
								if !ok || !v {
									allActivated = false
									break
								}
							}
						}
						newState := !allActivated
						for _, ch := range g.Channels {
							if ch.Type == discordgo.ChannelTypeGuildText {
								cidInt, _ := discordIDStringToInt64(ch.ID)
								channelStates.Lock()
								channelStates.m[cidInt] = newState
								channelStates.Unlock()
								_ = updateChannelState(db, cidInt, newState)
							}
						}

						// Build updated toggle button reflecting new overall state
						label := "Activated"
						style := discordgo.SuccessButton
						if !newState {
							label = "Deactivated"
							style = discordgo.DangerButton
						}
						btn := &discordgo.Button{
							CustomID: "toggle_fixembed",
							Label:    label,
							Style:    style,
						}
						components := []discordgo.MessageComponent{
							&discordgo.ActionsRow{Components: []discordgo.MessageComponent{btn}},
						}
						embed := &discordgo.MessageEmbed{Title: "FixEmbed Settings", Description: "Toggled FixEmbed for guild channels.", Color: 0x00ff00}
						_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
							Type: discordgo.InteractionResponseUpdateMessage,
							Data: &discordgo.InteractionResponseData{
								Embeds:     []*discordgo.MessageEmbed{embed},
								Components: components,
							},
						})

						break
					}
				}
			} else {
				embed := &discordgo.MessageEmbed{Title: "FixEmbed Settings", Description: "Toggled FixEmbed for guild channels.", Color: 0x00ff00}
				_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseUpdateMessage,
					Data: &discordgo.InteractionResponseData{
						Embeds: []*discordgo.MessageEmbed{embed},
					},
				})
			}
		}
	}
}

// Helper: discord ID string to int64
func discordIDStringToInt64(s string) (int64, error) {
	// discordgo provides helper in Snowflake types; but we can parse directly
	// Accept strings like "123456789012345678"
	var id int64
	_, err := fmt.Sscan(s, &id)
	return id, err
}

func onMessageCreate(db *sql.DB, s *discordgo.Session, m *discordgo.MessageCreate) {
	// ignore own messages
	if s.State.User != nil && m.Author.ID == s.State.User.ID {
		return
	}
	if m.GuildID == "" {
		return
	}
	gidInt, _ := discordIDStringToInt64(m.GuildID)

	// Debug: log incoming message for troubleshooting link processing
	log.Printf("[DEBUG] onMessageCreate: guild=%s channel=%s author=%s content=%q", m.GuildID, m.ChannelID, m.Author.ID, m.Content)

	// fetch guild settings or defaults
	botSettings.RLock()
	settings := botSettings.m[gidInt]
	botSettings.RUnlock()
	if settings == nil {
		settings = &GuildSettings{EnabledServices: defaultServices(), MentionUsers: true, DeleteOriginal: true}
	}

	enabledServices := settings.EnabledServices
	mentionUsers := settings.MentionUsers
	deleteOriginal := settings.DeleteOriginal

	// Debug: log effective guild settings
	log.Printf("[DEBUG] onMessageCreate: guildSettings enabledServices=%v mentionUsers=%t deleteOriginal=%t", enabledServices, mentionUsers, deleteOriginal)

	// Check if bot enabled in this channel
	cidInt, _ := discordIDStringToInt64(m.ChannelID)
	channelStates.RLock()
	enabled, ok := channelStates.m[cidInt]
	channelStates.RUnlock()
	// Debug: log channel state
	log.Printf("[DEBUG] onMessageCreate: channelState ok=%t enabled=%t cid=%d", ok, enabled, cidInt)
	if ok && !enabled {
		// deactivated for this channel
		log.Printf("[DEBUG] onMessageCreate: channel is deactivated, skipping message")
		return
	}

	// Patterns from Python ported
	linkPattern := `https?://(?:www\.)?(twitter\.com/[A-Za-z0-9_]+/status/[0-9]+|x\.com/[A-Za-z0-9_]+/status/[0-9]+|instagram\.com/(?:p|reel)/[A-Za-z0-9_-]+|reddit\.com/r/[A-Za-z0-9_]+/s/[A-Za-z0-9_]+|reddit\.com/r/[A-Za-z0-9_]+/comments/[A-Za-z0-9_]+/[A-Za-z0-9_]+|old\.reddit\.com/r/[A-Za-z0-9_]+/comments/[A-Za-z0-9_]+/[A-Za-z0-9_]+|pixiv\.net/(?:en/)?artworks/[0-9]+|threads\.(?:net|com)/@[^/]+/post/[A-Za-z0-9_-]+|bsky\.app/profile/[^/]+/post/[A-Za-z0-9_-]+)`
	surroundedPattern := `<https?://(?:www\.)?(twitter\.com/[A-Za-z0-9_]+/status/[0-9]+|x\.com/[A-Za-z0-9_]+/status/[0-9]+|instagram\.com/(?:p|reel)/[A-Za-z0-9_-]+|reddit\.com/r/[A-Za-z0-9_]+/s/[A-Za-z0-9_]+|reddit\.com/r/[A-Za-z0-9_]+/comments/[A-Za-z0-9_]+/[A-Za-z0-9_]+|old\.reddit\.com/r/[A-Za-z0-9_]+/comments/[A-Za-z0-9_]+/[A-Za-z0-9_]+|pixiv\.net/(?:en/)?artworks/[0-9]+|threads\.(?:net|com)/@[^/]+/post/[A-Za-z0-9_-]+|bsky\.app/profile/[^/]+/post/[A-Za-z0-9_-]+)>`

	// Debug: show the regex patterns we're using
	log.Printf("[DEBUG] onMessageCreate: linkPattern=%q surroundedPattern=%q", linkPattern, surroundedPattern)
	reLink := regexp.MustCompile(linkPattern)
	reSurrounded := regexp.MustCompile(surroundedPattern)

	matches := reLink.FindAllStringSubmatch(m.Content, -1)
	if len(matches) == 0 {
		log.Printf("[DEBUG] onMessageCreate: no link matches in message")
		// also log whether the message contains a surrounded link (which we skip)
		if reSurrounded.MatchString(m.Content) {
			log.Printf("[DEBUG] onMessageCreate: message contains surrounded link; skipping per design")
		}
		return
	}
	log.Printf("[DEBUG] onMessageCreate: found %d link match(es)", len(matches))
	// Log each match and its capture groups for diagnostics
	for mi, mmatch := range matches {
		if len(mmatch) == 0 {
			continue
		}
		log.Printf("[DEBUG] onMessageCreate: match[%d].full=%q", mi, mmatch[0])
		for gi, g := range mmatch {
			log.Printf("[DEBUG] onMessageCreate: match[%d].group[%d]=%q", mi, gi, g)
		}
	}

	// If any link is surrounded by <...>, skip processing entirely (per original logic which checks per message)
	if reSurrounded.MatchString(m.Content) {
		return
	}

	for _, match := range matches {
		// match[1] is the captured domain/... part like "twitter.com/user/status/123"
		originalLink := match[1]
		if originalLink == "" {
			continue
		}
		displayText := ""
		modifiedLink := originalLink
		service := ""
		userOrCommunity := ""

		// domain is first component before slash
		parts := strings.Split(originalLink, "/")
		domain := strings.ToLower(parts[0])

		switch domain {
		case "twitter.com", "x.com":
			service = "Twitter"
			re := regexp.MustCompile(`(?:twitter\.com|x\.com)/([A-Za-z0-9_]+)/status/[0-9]+`)
			mm := re.FindStringSubmatch(originalLink)
			if len(mm) > 1 {
				userOrCommunity = mm[1]
			} else {
				userOrCommunity = "Unknown"
			}
		case "instagram.com":
			service = "Instagram"
			re := regexp.MustCompile(`instagram\.com/(?:p|reel)/([A-Za-z0-9_-]+)`)
			mm := re.FindStringSubmatch(originalLink)
			if len(mm) > 1 {
				userOrCommunity = mm[1]
			} else {
				userOrCommunity = "Unknown"
			}
		case "reddit.com", "old.reddit.com":
			service = "Reddit"
			re := regexp.MustCompile(`(?:reddit\.com|old\.reddit\.com)/r/([A-Za-z0-9_]+)`)
			mm := re.FindStringSubmatch(originalLink)
			if len(mm) > 1 {
				userOrCommunity = mm[1]
			} else {
				userOrCommunity = "Unknown"
			}
		case "pixiv.net":
			service = "Pixiv"
			re := regexp.MustCompile(`pixiv\.net/(?:en/)?artworks/([0-9]+)`)
			mm := re.FindStringSubmatch(originalLink)
			if len(mm) > 1 {
				userOrCommunity = mm[1]
			} else {
				userOrCommunity = "Unknown"
			}
		case "threads.net", "threads.com":
			service = "Threads"
			re := regexp.MustCompile(`threads\.(?:net|com)/@([^/]+)/post/([A-Za-z0-9_-]+)`)
			mm := re.FindStringSubmatch(originalLink)
			if len(mm) > 2 {
				userOrCommunity = mm[1]
				postID := mm[2]
				modifiedLink = fmt.Sprintf("fixthreads.net/@%s/post/%s", userOrCommunity, postID)
				displayText = fmt.Sprintf("Threads • @%s", userOrCommunity)
			}
		case "bsky.app":
			service = "Bluesky"
			re := regexp.MustCompile(`bsky\.app/profile/([^/]+)/post/([A-Za-z0-9_-]+)`)
			mm := re.FindStringSubmatch(originalLink)
			if len(mm) > 2 {
				userOrCommunity = mm[1]
				postID := mm[2]
				modifiedLink = fmt.Sprintf("bskyx.app/profile/%s/post/%s", userOrCommunity, postID)
				displayText = fmt.Sprintf("Bluesky • %s", userOrCommunity)
			}
		}

		// verify enabled
		if service != "" && userOrCommunity == "" {
			// when we didn't set userOrCommunity for threads/bluesky (already handled above), fall back:
			parts2 := strings.Split(originalLink, "/")
			if len(parts2) > 1 {
				userOrCommunity = parts2[1]
			} else {
				userOrCommunity = "Unknown"
			}
		}

		// check if service is enabled for this guild
		enabled := false
		for _, sname := range enabledServices {
			if sname == service {
				enabled = true
				break
			}
		}
		if service != "" && userOrCommunity != "" && !enabled {
			log.Printf("[DEBUG] onMessageCreate: service %s is not enabled for this guild (enabledServices=%v)", service, enabledServices)
		}

		if service != "" && userOrCommunity != "" && enabled {
			if displayText == "" {
				displayText = fmt.Sprintf("%s • %s", service, userOrCommunity)
			}
			// apply domain replacements
			switch service {
			case "Twitter":
				modifiedLink = strings.ReplaceAll(originalLink, "twitter.com", "fxtwitter.com")
				modifiedLink = strings.ReplaceAll(modifiedLink, "x.com", "fixupx.com")
			case "Instagram":
				modifiedLink = strings.ReplaceAll(originalLink, "instagram.com", "instafix.ldez.top")
			case "Reddit":
				if strings.Contains(originalLink, "old.reddit.com") {
					modifiedLink = strings.ReplaceAll(originalLink, "old.reddit.com", "old.rxddit.com")
				} else {
					modifiedLink = strings.ReplaceAll(originalLink, "reddit.com", "vxreddit.ldez.workers.dev")
				}
			case "Threads":
				modifiedLink = strings.ReplaceAll(originalLink, "threads.net", "fixthreads.net")
				modifiedLink = strings.ReplaceAll(modifiedLink, "threads.com", "fixthreads.net")
			case "Pixiv":
				modifiedLink = strings.ReplaceAll(originalLink, "pixiv.net", "phixiv.net")
			case "Bluesky":
				modifiedLink = strings.ReplaceAll(originalLink, "bsky.app", "fxbsky.app")
			}

			formattedMessage := fmt.Sprintf("[%s](https://%s)", displayText, modifiedLink)
			if mentionUsers {
				formattedMessage = formattedMessage + fmt.Sprintf(" | Sent by <@%s>", m.Author.ID)
			} else {
				formattedMessage = formattedMessage + fmt.Sprintf(" | Sent by %s", m.Author.Username)
			}

			// Debug: log the rewritten message before sending
			log.Printf("[DEBUG] onMessageCreate: original=%s service=%s userOrCommunity=%s modified=%s formatted=%s deleteOriginal=%t", originalLink, service, userOrCommunity, modifiedLink, formattedMessage, deleteOriginal)

			if deleteOriginal {
				_ = rateLimitedSend(s, m.ChannelID, formattedMessage)
				_ = s.ChannelMessageDelete(m.ChannelID, m.ID)
			} else {
				// Attempt to suppress embeds on the original message (set SUPPRESS_EMBEDS flag)
				// In Discord, SUPPRESS_EMBEDS == 4
				// discordgo MessageEdit.Flags is discordgo.MessageFlags; construct value accordingly
				flags := discordgo.MessageFlags(1 << 2)
				_, _ = s.ChannelMessageEditComplex(&discordgo.MessageEdit{
					ID:      m.ID,
					Channel: m.ChannelID,
					Content: &m.Content,
					Flags:   flags,
				})
				_ = rateLimitedSend(s, m.ChannelID, formattedMessage)
			}
		}
	}
}

func onGuildCreate(db *sql.DB, s *discordgo.Session, g *discordgo.GuildCreate) {
	// when joining a guild, create default settings if missing
	if g.Guild.ID == "" {
		return
	}
	gidInt, _ := discordIDStringToInt64(g.Guild.ID)
	botSettings.Lock()
	if _, ok := botSettings.m[gidInt]; !ok {
		botSettings.m[gidInt] = &GuildSettings{
			EnabledServices: defaultServices(),
			MentionUsers:    true,
			DeleteOriginal:  true,
		}
		_ = updateSetting(db, gidInt, botSettings.m[gidInt].EnabledServices, true, true)
	}
	botSettings.Unlock()
}

func startStatusRotator(s *discordgo.Session, stop <-chan struct{}) {
	ticker := time.NewTicker(60 * time.Second)
	idx := 0
	// set initial presence immediately
	_ = updateStatus(s, statuses[idx])
	for {
		select {
		case <-ticker.C:
			idx = (idx + 1) % len(statuses)
			_ = updateStatus(s, statuses[idx])
		case <-stop:
			ticker.Stop()
			return
		}
	}
}

func updateStatus(s *discordgo.Session, text string) error {
	act := &discordgo.Activity{
		Name: text,
		Type: discordgo.ActivityTypeWatching,
	}
	return s.UpdateStatusComplex(discordgo.UpdateStatusData{
		Activities: []*discordgo.Activity{act},
	})
}

func main() {
	// Load .env
	_ = godotenv.Load()
	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		log.Fatalln("BOT_TOKEN is not set in environment")
	}
	// single owner ID for owner-only commands (set via OWNER_ID environment variable)
	ownerID = os.Getenv("OWNER_ID")
	if ownerID == "" {
		log.Println("Warning: OWNER_ID is not set; owner-only command will be disabled")
	}

	db, err := initDB("fixembed_data.db")
	if err != nil {
		log.Fatalf("DB init error: %v", err)
	}
	defer db.Close()

	intents := discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent | discordgo.IntentsGuilds
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}
	dg.Identify.Intents = intents

	// Add handlers
	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("We have logged in as %s", s.State.User.Username)
		// load channel states and settings now that session.State is populated
		if err := loadChannelStates(db, s); err != nil {
			log.Printf("Error loading channel states: %v", err)
		}
		if err := loadSettings(db); err != nil {
			log.Printf("Error loading settings: %v", err)
		}

		// Register application commands per-guild to mirror Python client.tree.sync behaviour.
		commands := []*discordgo.ApplicationCommand{
			{
				Name:        "activate",
				Description: "Activate link processing in this channel or another channel",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionChannel,
						Name:        "channel",
						Description: "The channel to activate link processing in (leave blank for current channel)",
						Required:    false,
					},
				},
			},
			{
				Name:        "deactivate",
				Description: "Deactivate link processing in this channel or another channel",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionChannel,
						Name:        "channel",
						Description: "The channel to deactivate link processing in (leave blank for current channel)",
						Required:    false,
					},
				},
			},
			{
				Name:        "about",
				Description: "Show information about the bot",
			},
			{
				Name:        "settings",
				Description: "Configure FixEmbed's settings",
			},
			{
				Name:        "owner",
				Description: "Owner-only command: lists guilds the bot is in",
			},
		}

		created := 0
		// Force-sync commands for each guild to avoid duplicates left from previous runs.
		// ApplicationCommandBulkOverwrite replaces the guild's commands with exactly `commands`.
		for _, g := range s.State.Guilds {
			_, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, g.ID, commands)
			if err != nil {
				log.Printf("Warning: failed to sync commands in guild %s: %v", g.ID, err)
			} else {
				created++
			}
		}
		log.Printf("Synchronized commands across %d guild(s)", created)
	})

	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		onInteractionCreate(db, s, i)
	})
	dg.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		onMessageCreate(db, s, m)
	})
	dg.AddHandler(func(s *discordgo.Session, g *discordgo.GuildCreate) {
		onGuildCreate(db, s, g)
	})

	// Open websocket
	if err := dg.Open(); err != nil {
		log.Fatalf("Cannot open the session: %v", err)
	}
	defer dg.Close()

	// Commands are registered per-guild in the Ready handler (mirrors Python client.tree.sync()).

	// Start status rotator
	stopStatus := make(chan struct{})
	go startStatusRotator(dg, stopStatus)

	// Wait for CTRL-C or SIGTERM
	log.Println("Bot is now running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	// Cleanup
	close(stopStatus)
	log.Println("Shutting down.")
}

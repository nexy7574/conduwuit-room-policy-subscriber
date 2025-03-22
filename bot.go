package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	ubotUtil "github.com/nexy7574/ubot/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	configPath     = flag.String("config", "config.json", "Path to config file")
	dataDir        = flag.String("data-dir", "", "Path to data directory (overrides config)")
	logLevel       = flag.String("log-level", "", "Log level (overrides config)")
	dryRun         = flag.Bool("dry-run", false, "Don't issue any bans, just log them")
	generateConfig = flag.Bool("generate-config", false, "Generate a config file with defaults")
)

type Config struct {
	AccessToken   string      `json:"access_token"`
	Homeserver    string      `json:"homeserver"`
	AdminRoomID   id.RoomID   `json:"admin_room"`
	ListenTo      []id.RoomID `json:"listen_to"`
	LogLevel      string      `json:"log_level"`
	LegacyVersion bool        `json:"legacy_version"`
	DataDir       string      `json:"data_dir"`
}

func (c *Config) RealLogLevel() string {
	if *logLevel != "" {
		return *logLevel
	}
	return c.LogLevel
}

func (c *Config) RealDataDir() string {
	if *dataDir != "" {
		log.Trace().Str("config", c.DataDir).Str("flag", *dataDir).
			Msg("Overriding configured data dir with runtime flag")
		return *dataDir
	}
	return c.DataDir
}

func LoadConfig(path string) (config *Config, err error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(content, &config); err != nil {
		return nil, err
	}
	return config, err
}

type Bot struct {
	Mau      *mautrix.Client
	Config   *Config
	BanLock  *sync.Mutex
	ProxyAPI *http.Client
}
type ExistingBans struct {
	Existing []id.RoomID `json:"existing"`
}
type ProxyAPIRoom struct {
	RoomID  id.RoomID `json:"room_id"`
	Members int       `json:"members"`
	Name    string    `json:"name"`
}

func (p *ProxyAPIRoom) HashedRoomID() string {
	return hex.EncodeToString(sha256.New().Sum([]byte(p.RoomID.String())))
}

func (bot *Bot) FindRoomWithHash(entity string) *id.RoomID {
	page := 0
	for {
		response, err := bot.ProxyAPI.Get(bot.Config.Homeserver + "/_conduwuit/rooms/list?page=" + fmt.Sprint(page))
		if err != nil {
			log.Error().Err(err).Msg("failed to fetch room list")
			return nil
		}
		var responseBody []ProxyAPIRoom
		err = json.NewDecoder(response.Body).Decode(&responseBody)
		closeError := response.Body.Close()
		if closeError != nil {
		} // literally do not care, shut up linter
		if err != nil {
			log.Error().Err(err).Msg("failed to decode room list")
			return nil
		}
		if len(responseBody) == 0 {
			break
		}
		for _, room := range responseBody {
			if room.HashedRoomID() == entity {
				roomID := room.RoomID
				return &roomID
			}
		}
	}
	return nil
}

func (bot *Bot) OnPolicyEvent(ctx context.Context, evt *event.Event) {
	bot.BanLock.Lock()
	defer bot.BanLock.Unlock()
	logger := log.With().Str("room_id", evt.RoomID.String()).Str("event_id", evt.ID.String()).Logger()
	if !slices.Contains(bot.Config.ListenTo, evt.RoomID) {
		logger.Trace().Msg("ignoring event from room I don't care about")
		return
	}
	if evt.Type != event.StatePolicyRoom {
		logger.Trace().Msg("ignoring non room policy event")
		return
	}
	content := evt.Content.AsModPolicy()
	if content == nil {
		logger.Warn().Interface("event", evt).Msg("event doesn't have content!")
		return
	}
	if content.Entity == "" && content.UnstableHashes != nil {
		logger.Warn().Str("entity", content.EntityOrHash()).Msg(
			"Received a hashed policy list event. Trying to resolve it...")
		resolvedRoomID := bot.FindRoomWithHash(content.EntityOrHash())
		if resolvedRoomID == nil {
			logger.Warn().Str("entity", content.EntityOrHash()).Msg("Failed to resolve hashed entity!")
			return
		}
		log.Info().Str("hashed_entity", content.EntityOrHash()).Str("resolved_entity", resolvedRoomID.String()).
			Msg("Resolved hashed entity")
		content.Entity = resolvedRoomID.String()
	}
	if !strings.HasPrefix(content.Entity, "!") {
		logger.Warn().Str("entity", content.Entity).Msg("entity doesn't look like a room ID!")
		return
	}

	_, _, targetHS := id.ParseCommonIdentifier(content.Entity)
	targetID := id.RoomID(content.Entity)

	var alreadyBannedRooms ExistingBans
	err := bot.Mau.GetAccountData(ctx, "com.github.nexy7574.conduwuit-room-policy-subscriber.banned-rooms", &alreadyBannedRooms)
	if err != nil {
		log.Error().Err(err).Msg("failed to get banned rooms")
	} else {
		if slices.Contains(alreadyBannedRooms.Existing, targetID) {
			logger.Info().Str("entity", targetID.String()).Msg("room already banned")
			return
		}
	}

	vias := []string{bot.Mau.UserID.Homeserver(), evt.Sender.Homeserver(), targetHS, "matrix.org"}
	summary, err := bot.Mau.GetRoomSummary(ctx, targetID.String(), vias...)
	if err != nil {
		summary = &mautrix.RespRoomSummary{PublicRoomInfo: mautrix.PublicRoomInfo{RoomID: targetID}}
	}

	plRoom := Room{Room: *mautrix.NewRoom(evt.RoomID), bot: bot}
	plRoom.Load()
	notice := fmt.Sprintf(
		"Banning room %s (%s) due to [this policy event](%s) in %s, which states:\n<blockquote>%s</blockquote>",
		PillSummary(*summary), targetID, evt.RoomID.EventURI(evt.ID, vias...), plRoom.Pill(), content.Reason,
	)
	noticeEvent := format.RenderMarkdown(notice, true, true)
	noticeEvent.MsgType = "m.notice"
	_, _ = bot.Mau.SendMessageEvent(ctx, bot.Config.AdminRoomID, event.EventMessage, noticeEvent)

	command := "!admin rooms moderation ban-room " + targetID.String()
	if bot.Config.LegacyVersion {
		command = "!admin rooms moderation ban-room --force --disable-federation " + targetID.String()
	}
	if *dryRun {
		command = "+" + command
	}
	_, err = bot.Mau.SendText(ctx, bot.Config.AdminRoomID, command)
	if err == nil {
		alreadyBannedRooms.Existing = append(alreadyBannedRooms.Existing, targetID)
		err = bot.Mau.SetAccountData(ctx, "com.github.nexy7574.conduwuit-room-policy-subscriber.banned-rooms", alreadyBannedRooms)
		if err != nil {
			log.Error().Err(err).Msg("failed to save banned rooms")
		}
	}
}

func main() {
	flag.Parse()
	logLevelParsed, err := zerolog.ParseLevel(*logLevel)
	if err != nil {
		panic(err)
	}
	log.Logger = log.Level(logLevelParsed).Output(zerolog.ConsoleWriter{Out: os.Stderr})

	if *generateConfig {
		config := Config{}
		data, err := json.MarshalIndent(config, "", "  ")
		if err != nil {
			log.Fatal().Err(err).Msg("failed to generate config")
		}
		err = os.WriteFile(*configPath, data, 0644)
		if err != nil {
			log.Fatal().Err(err).Msg("failed to write config")
		}
		log.Info().Str("config_path", *configPath).Msg("generated config")
		return
	}

	ctx := context.Background()
	config, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatal().Err(err).Str("config_path", *configPath).Msg("failed to load config!")
	}
	bot := &Bot{Config: config, BanLock: &sync.Mutex{}}

	mau, err := mautrix.NewClient(config.Homeserver, "", config.AccessToken)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create client")
	}
	mau.Log = log.Logger
	bot.Mau = mau
	whoAmIResp, err := bot.Mau.Whoami(ctx)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to call whoami (invalid token?)")
	}
	bot.Mau.UserID = whoAmIResp.UserID
	bot.Mau.DeviceID = whoAmIResp.DeviceID
	log.Info().Msgf("Logged in as %s (device ID: %s)", whoAmIResp.UserID, whoAmIResp.DeviceID)
	store, storeErr := ubotUtil.NewFileStore(".", bot.Mau)
	if storeErr != nil {
		log.Fatal().Err(storeErr).Msg("failed to create store")
	}
	bot.Mau.Store = store
	if os.Getenv("RESET_NEXT_BATCH") != "" {
		log.Warn().Msg("Resetting next batch for an initial sync")
		err := bot.Mau.Store.SaveNextBatch(ctx, bot.Mau.UserID, "0")
		if err != nil {
			log.Error().Err(err).Msg("failed to save next batch")
		}
	}
	if os.Getenv("RESET_BAN_LIST") != "" {
		log.Warn().Msg("Resetting stored bans")
		err := bot.Mau.SetAccountData(ctx, "com.github.nexy7574.conduwuit-room-policy-subscriber.banned-rooms", ExistingBans{})
		if err != nil {
			log.Error().Err(err).Msg("failed to save banned rooms")
		}
	}
	filter := mautrix.DefaultFilter()
	filter.Room.Rooms = config.ListenTo
	filter.Room.Timeline.Rooms = config.ListenTo
	filter.Room.State.Rooms = config.ListenTo

	syncer := bot.Mau.Syncer.(*mautrix.DefaultSyncer)
	syncer.FilterJSON = &filter
	syncer.OnEventType(event.StatePolicyRoom, bot.OnPolicyEvent)
	syncer.OnSync(func(ctx context.Context, resp *mautrix.RespSync, since string) bool {
		log.Trace().Interface("resp", resp).Msg("Synced")
		log.Trace().Str("since", since).Str("next_batch", resp.NextBatch).Msg("Completed sync.")
		return true
	})

	httpClient := &http.Client{Timeout: time.Second * 30}
	bot.ProxyAPI = httpClient

	log.Info().Msg("Synchronising ban states.")
	for _, roomID := range config.ListenTo {
		_, err = bot.Mau.JoinRoomByID(ctx, roomID)
		if err != nil {
			log.Fatal().Err(err).Str("room_id", roomID.String()).Msg("failed to join room")
		}
		state, err := bot.Mau.State(ctx, roomID)
		if err != nil {
			log.Fatal().Err(err).Str("room_id", roomID.String()).Msg("failed to get state to backfill bans")
		}
		for _, evt := range state[event.StatePolicyRoom] {
			bot.OnPolicyEvent(ctx, evt)
		}
	}
	log.Info().Msg("Starting to sync.")
	if err = bot.Mau.SyncWithContext(ctx); err != nil {
		//log.Fatal().Err(err).Msg("failed to sync")
		panic(err)
	}
}

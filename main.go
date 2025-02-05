package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
	"os"
	"strings"
	"sync"
	"time"
)

var homeserver = flag.String("url", "https://matrix-client.matrix.org", "The URL of the homeserver to connect to")
var accessToken = flag.String("token", "", "The access token to use for the connection")
var adminRoomAlias = flag.String("room", "", "The alias of the admin room")
var resolvedAdminRoomID id.RoomID
var dryRun = flag.Bool("dry-run", false, "Don't actually send any messages")
var defederate = flag.Bool("defederate", false, "Ban federation of the room after banning")
var startupTimestamp = time.Now().UnixMilli()

func sendRoomBan(ctx context.Context, client *mautrix.Client, policyEvent *event.Event) []error {
	log.Trace().
		Stringer("admin_room", resolvedAdminRoomID).
		Interface("policy_event", policyEvent).
		Msg("Processing policy event")
	if strings.HasPrefix(*policyEvent.StateKey, "room") {
		log.Debug().Str("state_key", *policyEvent.StateKey).Msg("Ignoring legacy room policy event")
		return nil
	}
	sendErrors := make([]error, 0)
	rawTargetRoomId := policyEvent.Content.Raw["entity"]
	if rawTargetRoomId == nil {
		log.Warn().Interface("content", policyEvent.Content).Msg("No entity found in policy event")
		return nil
	}
	targetRoomId := rawTargetRoomId.(string)

	summary, err := client.GetRoomSummary(ctx, targetRoomId)
	var roomName string
	if err != nil {
		log.Error().Str("room", targetRoomId).Err(err).Msg("Error getting room summary")
		roomName = targetRoomId
	} else {
		roomName = summary.Name
		if roomName == "" {
			fallbackName := summary.CanonicalAlias.String()
			if fallbackName == "" {
				log.Warn().Str("room", targetRoomId).Msg("No name or alias found for room, using room ID as name")
				fallbackName = targetRoomId
			}
			roomName = fallbackName
		}
	}

	commandParts := []string{"!admin", "rooms", "moderation", "ban-room"}
	if *defederate {
		commandParts = append(commandParts, "--disable-federation")
	}
	commandParts = append(commandParts, targetRoomId)
	actionName := "Banning"
	if *defederate {
		actionName += " and defederating"
	}

	messagesToSend := [2]string{
		fmt.Sprintf(
			" %s room [%s](matrix:roomid/%s): `%s`",
			actionName,
			roomName,
			targetRoomId,
			policyEvent.Content.Raw["reason"],
		),
		strings.Join(commandParts, " "),
	}
	lastEvent := id.EventID("")

	for _, message := range messagesToSend {
		isNotice := strings.HasPrefix(message, " ")
		if *dryRun {
			message = fmt.Sprintf("[DRY-RUN] %s", message)
		}

		body := format.RenderMarkdown(message, true, true)
		if isNotice {
			body.MsgType = event.MsgNotice
		}
		if lastEvent.String() != "" {
			body.RelatesTo = &event.RelatesTo{
				InReplyTo: &event.InReplyTo{EventID: lastEvent},
			}
		}
		log.Info().Str("room_id", commandParts[len(commandParts)-1]).Msg("Banning room")
		resp, err := client.SendMessageEvent(
			ctx,
			resolvedAdminRoomID,
			event.EventMessage,
			body,
		)
		if err != nil {
			log.Error().Err(err).Msg("Error sending message")
			sendErrors = append(sendErrors, err)
		} else {
			log.Debug().
				Stringer("admin_room", resolvedAdminRoomID).
				Stringer("event_id", resp.EventID).
				Msg("Sent message in admin room")
			lastEvent = resp.EventID
		}
		lastEvent = resp.EventID
	}
	return sendErrors
}

func handleIncomingCommand(ctx context.Context, db *DBHelper, client *mautrix.Client, evt *event.Event) {
	message := evt.Content.AsMessage()
	if message == nil {
		log.Debug().Interface("event", evt).Msg("Ignoring non-message event")
		return
	}
	if message.MsgType != event.MsgText {
		log.Debug().Interface("message", message).Msg("Ignoring non-text message")
		return
	}
	args := strings.Split(message.Body, " ")
	if !strings.HasPrefix(args[0], "?") {
		log.Debug().Str("command", args[0]).Msg("Ignoring non-admin command")
		return
	}
	command := args[0][1:]
	switch command {
	case "ping":
		body := format.RenderMarkdown("Pong!", true, true)
		body.MsgType = event.MsgNotice
		body.RelatesTo = &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: evt.ID}}
		_, err := client.SendMessageEvent(
			ctx,
			evt.RoomID,
			event.EventMessage,
			body,
		)
		if err != nil {
			log.Error().Err(err).Msg("Error sending message")
		}
	case "backfill":
		if len(args) < 2 {
			log.Debug().Interface("args", args).Msg("Ignoring backfill command with no room ID")
			return
		}
		targetRoomId := id.RoomID(args[1])
		log.Info().Str("room_id", targetRoomId.String()).Msg("Backfilling events from room")
		_, err := client.SendNotice(ctx, evt.RoomID, fmt.Sprintf("Backfilling events from room %s", targetRoomId))
		if err != nil {
			log.Error().Err(err).Msg("Error sending notice")
		}
		// fetch history and manually dispatch
		nextBatch := ""
		processedEvents := 0
		for {
			events, err := client.Messages(
				ctx,
				targetRoomId,
				nextBatch,
				"",
				mautrix.DirectionBackward,
				&mautrix.FilterPart{
					Limit: 100,
					Types: []event.Type{event.StatePolicyRoom},
				},
				100,
			)
			if err != nil {
				log.Error().Err(err).Msg("Error fetching events")
				break
			}
			for _, evt2 := range events.Chunk {
				processPolicyRoomEvent(*db, client)(ctx, evt2)
				processedEvents++
			}
			if events.End == "" {
				break
			}
			nextBatch = events.End
		}
		log.Info().Int("count", processedEvents).Msg("Backfilled events")
		_, err = client.SendNotice(ctx, evt.RoomID, fmt.Sprintf("Backfilled %d events from room %s", processedEvents, targetRoomId))
		if err != nil {
			log.Error().Err(err).Msg("Error sending notice")
		}
	case "help":
		body := format.RenderMarkdown(
			"Available commands:\n"+
				" - `?ping`: Responds with 'Pong!'\n"+
				" - `?backfill <room_id>`: Backfills events from the specified room\n"+
				" - `?help`: Shows this help message",
			true,
			true,
		)
		body.MsgType = event.MsgNotice
		body.RelatesTo = &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: evt.ID}}
		_, err := client.SendMessageEvent(
			ctx,
			evt.RoomID,
			event.EventMessage,
			body,
		)
		if err != nil {
			log.Error().Err(err).Msg("Error sending message")
		}
	default:
		log.Debug().Str("command", command).Msg("Ignoring unknown command")
	}
}

func processMessageEvent(bansDB DBHelper, client *mautrix.Client) func(ctx context.Context, event *event.Event) {
	return func(ctx context.Context, event *event.Event) {
		if event.Timestamp < startupTimestamp {
			log.Debug().Interface("event", event).Msg("Ignoring old event")
			return
		}
		if event.RoomID.String() == resolvedAdminRoomID.String() {
			handleIncomingCommand(ctx, &bansDB, client, event)
		}
	}
}

func processPolicyRoomEvent(bansDB DBHelper, client *mautrix.Client) func(ctx context.Context, evt *event.Event) {
	return func(ctx context.Context, evt *event.Event) {
		if evt.Type != event.StatePolicyRoom {
			log.Debug().Interface("event", evt).Msg("Ignoring non-room-policy room event")
			return
		}
		exists, err := bansDB.GetBan(evt.RoomID.String())
		if exists.ID.String() != "" {
			log.Trace().Str("room", evt.RoomID.String()).Msg("Ban already exists for room.")
			return
		}
		if err != nil {
			// If the error is just "not found", ignore.
			if !errors.Is(err, sql.ErrNoRows) {
				log.Error().Err(err).Msg("Error getting ban from DB")
			} else {
				log.Debug().Str("room", evt.RoomID.String()).Msg("No ban found in DB")
			}
		} else {
			log.Debug().Str("room", evt.RoomID.String()).Msg("Ban found in DB")
			return
		}
		log.Info().
			Str("room", evt.RoomID.String()).
			Interface("content", evt).
			Msg("Got policy rule evt")
		sendErrors := sendRoomBan(ctx, client, evt)
		if len(sendErrors) > 0 {
			log.Error().Errs("errors", sendErrors).Msg("Errors sending messages")
		}
		err = bansDB.AddBan(evt.RoomID.String(), *evt)
		if err != nil {
			log.Error().Err(err).Msg("Error adding ban to DB")
		}
	}
}

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}
	logLevelParsed, err := zerolog.ParseLevel(logLevel)
	if err != nil {
		panic(err)
	}
	log.Logger = log.Level(logLevelParsed).Output(zerolog.ConsoleWriter{Out: os.Stderr})
	flag.Parse()
	ctx := context.Background()
	log.Trace().
		Str("homeserver", *homeserver).
		Str("token", *accessToken).
		Str("adminRoomAlias", *adminRoomAlias).
		Msg("Logging in and sending commands to admin room")
	client, err := mautrix.NewClient(*homeserver, "", *accessToken)
	if err != nil {
		panic(err)
	}

	// Get the user's own user ID
	log.Info().Msg("Getting user ID")
	userID, err := client.Whoami(ctx)
	if err != nil {
		panic(err)
	}
	client.UserID = userID.UserID
	log.Info().Str("userID", userID.UserID.String()).Msg("Found user ID")
	serverName := userID.UserID.Homeserver()
	if *adminRoomAlias == "" {
		log.Printf("No admin room alias provided, using default #admin:%s", serverName)
		*adminRoomAlias = fmt.Sprintf("#admin:%s", serverName)
	}
	roomId := *adminRoomAlias
	if strings.HasPrefix(*adminRoomAlias, "#") {
		// resolve alias to an ID
		log.Debug().Str("alias", *adminRoomAlias).Msg("Resolving admin room alias to a room ID")
		resolvedRoomId, err := client.ResolveAlias(ctx, id.RoomAlias(*adminRoomAlias))
		if err != nil {
			panic(err)
		}
		roomId = resolvedRoomId.RoomID.String()
		log.Info().Str("alias", *adminRoomAlias).Str("room_id", roomId).Msg("Resolved admin room alias to a room ID")
	} else if !strings.HasPrefix(*adminRoomAlias, "!") {
		panic("Invalid room ID or alias")
	}
	resolvedAdminRoomID = id.RoomID(roomId)

	syncer := client.Syncer.(*mautrix.DefaultSyncer)
	bansDB, err := NewDBHelper()
	if err != nil {
		panic(err)
	}
	syncer.OnEventType(event.StatePolicyRoom, processPolicyRoomEvent(*bansDB, client))
	syncer.OnEventType(event.EventMessage, processMessageEvent(*bansDB, client))
	// Start syncing
	syncCtx, cancelSync := context.WithCancel(ctx)
	var syncStopWait sync.WaitGroup
	syncStopWait.Add(1)

	log.Info().Msg("Starting sync...")
	err = client.SyncWithContext(syncCtx)
	if err != nil {
		panic(err)
	}
	cancelSync()
}

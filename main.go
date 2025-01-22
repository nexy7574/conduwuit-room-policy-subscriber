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
)

var homeserver = flag.String("url", "https://matrix-client.matrix.org", "The URL of the homeserver to connect to")
var accessToken = flag.String("token", "", "The access token to use for the connection")
var adminRoomAlias = flag.String("room", "", "The alias of the admin room")
var dryRun = flag.Bool("dry-run", false, "Don't actually send any messages")
var defederate = flag.Bool("defederate", false, "Ban federation of the room after banning")

func sendRoomBan(ctx context.Context, client *mautrix.Client, adminRoom id.RoomID, policyEvent *event.Event) []error {
	sendErrors := make([]error, 0)
	targetRoomId := policyEvent.Content.Raw["entity"].(string)
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
			adminRoom,
			event.EventMessage,
			body,
		)
		if err != nil {
			log.Error().Err(err).Msg("Error sending message")
			sendErrors = append(sendErrors, err)
		} else {
			log.Debug().
				Stringer("admin_room", adminRoom).
				Stringer("event_id", resp.EventID).
				Msg("Sent message in admin room")
			lastEvent = resp.EventID
		}
		lastEvent = resp.EventID
	}
	return sendErrors
}

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
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
	roomIdObj := id.RoomID(roomId)

	syncer := client.Syncer.(*mautrix.DefaultSyncer)
	bansDB, err := NewDBHelper()
	if err != nil {
		panic(err)
	}
	syncer.OnEventType(event.StatePolicyRoom, func(ctx context.Context, event *event.Event) {
		exists, err := bansDB.GetBan(event.RoomID.String())
		if exists.ID.String() != "" {
			log.Trace().Str("room", event.RoomID.String()).Msg("Ban already exists for room.")
			return
		}
		if err != nil {
			// If the error is just "not found", ignore.
			if !errors.Is(err, sql.ErrNoRows) {
				log.Error().Err(err).Msg("Error getting ban from DB")
			} else {
				log.Debug().Str("room", event.RoomID.String()).Msg("No ban found in DB")
			}
		}
		log.Info().
			Str("room", event.RoomID.String()).
			Interface("content", event).
			Msg("Got policy rule event")
		sendErrors := sendRoomBan(ctx, client, roomIdObj, event)
		if len(sendErrors) > 0 {
			log.Error().Errs("errors", sendErrors).Msg("Errors sending messages")
		}
		err = bansDB.AddBan(event.RoomID.String(), *event)
		if err != nil {
			log.Error().Err(err).Msg("Error adding ban to DB")
		}
	})
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

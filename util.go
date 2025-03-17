package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"slices"
)

type Room struct {
	mautrix.Room
	bot *Bot
}

func (room *Room) Load() *Room {
	state, err := room.bot.Mau.State(context.TODO(), room.ID)
	if err != nil {
		log.Warn().Err(err).Str("room_id", room.ID.String()).Msg("Failed to load room state")
		return room
	}
	// Is declaring it twice necessary?
	room.Room.State = state
	room.State = state
	return room
}

func (room *Room) GetHeroes() (heroes []id.UserID) {
	if room.bot == nil {
		return
	}
	var candidates []id.UserID
	// Filter for joined users first
	am, err := room.bot.Mau.StateStore.GetAllMembers(context.TODO(), room.ID)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get all members")
		return
	}
	for userID, member := range am {
		if !slices.Contains([]event.Membership{event.MembershipJoin, event.MembershipInvite}, member.Membership) {
			continue
		}
		if userID == room.bot.Mau.UserID {
			continue
		}
		candidates = append(candidates, userID)
	}
	if len(candidates) == 0 {
		// No joined users, just try anyone
		for userID := range am {
			if userID != room.bot.Mau.UserID {
				candidates = append(candidates, userID)
			}
		}
	}
	if len(candidates) > 0 {
		maxVal := 5
		if len(candidates) < maxVal {
			maxVal = len(candidates)
		}
		heroes = candidates[:maxVal]
	}

	return
}

func (room *Room) CalculatedName() string {
	name := room.GetStateContent(event.StateRoomName, "")
	if name != nil {
		evt := name.AsRoomName()
		if evt.Name != "" {
			return evt.Name
		}
	}
	canonicalAlias := room.GetStateContent(event.StateCanonicalAlias, "")
	if canonicalAlias != nil {
		evt := canonicalAlias.AsCanonicalAlias()
		if evt.Alias != "" {
			return evt.Alias.String()
		} else if len(evt.AltAliases) > 0 {
			return evt.AltAliases[0].String()
		}
	}

	if room.bot != nil {
		heroes := room.GetHeroes()
		if len(heroes) > 3 {
			return fmt.Sprintf("%s, %s, %s, and %d others", heroes[0], heroes[1], heroes[2], len(heroes)-3)
		} else if len(heroes) == 3 {
			return fmt.Sprintf("%s, %s and %s", heroes[0], heroes[1], heroes[2])
		} else if len(heroes) == 2 {
			return fmt.Sprintf("%s and %s", heroes[0], heroes[1])
		} else if len(heroes) == 1 {
			return string(heroes[0])
		} else {
			return "Empty room"
		}
	} else {
		return "Unnamed room"
	}
}

func (room *Room) GetStateContent(eventType event.Type, stateKey string) *event.Content {
	logger := log.With().
		Str("room_id", room.ID.String()).
		Str("event_type", eventType.String()).
		Str("state_key", stateKey).
		Logger()
	resp := room.GetStateEvent(eventType, stateKey)
	if resp == nil {
		logger.Warn().Msg("Failed to get state event")
		return &event.Content{}
	}
	err := resp.Content.ParseRaw(eventType)
	if err != nil && !errors.Is(err, event.ErrContentAlreadyParsed) {
		logger.Warn().Err(err).
			Interface("raw_content", resp.Content.Raw).
			Any("expected_type", eventType).
			Msg("Failed to parse state event raw content")
		return &event.Content{}
	}
	parsed := resp.Content.Parsed
	if parsed == nil {
		logger.Warn().Msg("Failed to parse state event content, was nil")
		return &event.Content{}
	}
	logger.Trace().Interface("parsed_content", parsed).Msg("Got state event content")
	return &resp.Content
}

func (room *Room) Pill() string {
	return fmt.Sprintf("[%s](%s)", room.String(), room.URI())
}

func (room *Room) String() string {
	name := room.CalculatedName()
	// || name == "Unnamed room" || name == "Empty room"
	if name == "" {
		return room.ID.String()
	}
	return name
}

func (room *Room) URI() *id.MatrixURI {
	canonicalAlias := room.GetStateContent(event.StateCanonicalAlias, "")
	if canonicalAlias != nil {
		alias := canonicalAlias.AsCanonicalAlias()
		if alias.Alias != "" {
			return alias.Alias.URI()
		} else if len(alias.AltAliases) > 0 {
			return alias.AltAliases[0].URI()
		}
	}
	heroes := room.GetHeroes()
	var vias []string
	if len(heroes) > 0 {
		for _, hero := range heroes {
			if !slices.Contains(vias, hero.Homeserver()) {
				vias = append(vias, hero.Homeserver())
			}
		}
	}
	return room.ID.URI(vias...)
}

func PillSummary(summary mautrix.RespRoomSummary) string {
	name := summary.Name
	if name == "" {
		if summary.CanonicalAlias != "" {
			name = summary.CanonicalAlias.String()
		} else {
			name = summary.RoomID.String()
		}
	}
	uriTarget := summary.RoomID.URI()
	if summary.CanonicalAlias != "" {
		uriTarget = summary.CanonicalAlias.URI()
	}
	return fmt.Sprintf("[%s](%s)", name, uriTarget)
}

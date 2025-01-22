package main

import (
	"database/sql"
	"encoding/json"
	_ "github.com/glebarez/go-sqlite"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix/event"
)

type DBHelper struct {
	db *sql.DB
}

func NewDBHelper() (*DBHelper, error) {
	db, err := sql.Open("sqlite", "./actions.db")
	if err != nil {
		return nil, err
	}
	_, err = db.Exec("CREATE TABLE IF NOT EXISTS bans (room_id TEXT PRIMARY KEY, event TEXT)")
	if err != nil {
		return nil, err
	}
	return &DBHelper{db: db}, nil
}

func (h *DBHelper) Close() error {
	return h.db.Close()
}

func (h *DBHelper) GetBan(roomID string) (origin event.Event, err error) {
	rawOrigin := ""
	log.Debug().Str("room_id", roomID).Msg("Getting ban for room from database")
	err = h.db.QueryRow("SELECT event FROM bans WHERE room_id = ?", roomID).Scan(&rawOrigin)
	if err != nil {
		log.Trace().Bytes("raw_origin", []byte(rawOrigin)).Err(err).Msg("Error getting ban for room from database")
		err = json.Unmarshal([]byte(rawOrigin), &origin)
		log.Debug().Any("origin", origin).Err(err).Msg("Unmarshaled ban for room from database")
	} else {
		log.Debug().Str("room_id", roomID).Bytes("raw_origin", []byte(rawOrigin)).Msg("Got ban for room from database")
	}
	if err != nil {
		log.Error().Str("room_id", roomID).Interface("origin", origin).Err(err).Msg("Error getting ban for room from database")
	} else {
		log.Debug().Str("room_id", roomID).Interface("origin", origin).Err(err).Msg("Got ban for room from database")
	}
	return origin, err
}

func (h *DBHelper) AddBan(roomID string, origin event.Event) error {
	// Event must be converted into JSON for dumping
	marshaled, err := json.Marshal(origin)
	if err != nil {
		return err
	}
	_, err = h.db.Exec(
		"INSERT INTO bans (room_id, event) VALUES (?, ?) ON CONFLICT (room_id) DO NOTHING",
		roomID,
		string(marshaled),
	)
	if err != nil {
		return err
	}
	return nil
}

func (h *DBHelper) RemoveBan(roomID string) error {
	_, err := h.db.Exec("DELETE FROM bans WHERE room_id = ?", roomID)
	return err
}

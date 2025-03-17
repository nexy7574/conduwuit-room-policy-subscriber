# conduwuit room policy subscriber

This is a simple tool that will sit in your conduwuit admin room and listen for
`m.policy.rule.room` events. When it receives one, it will send a command to the 
conduwuit admin room to ban the room, and optionally also ban federation with it.

## Installing

```bash
go install github.com/nexy7574/conduwuit-room-policy-subscriber@dev
```

Or, get the per-commit artifacts pre-built on
[nightly.link](https://nightly.link/nexy7574/conduwuit-room-policy-subscriber/workflows/build/dev?preview).

## Usage

```bash
Usage of ./conduwuit-room-policy-subscriber:
  -config string
        Path to config file (default "config.json")
  -dry-run
        Don't issue any bans, just log them
  -generate-config
        Generate a config file with defaults
  -log-level string
        Log level (overrides config) (default "info")
```

Example:

```shell
conduwuit-room-policy-subscriber -config ./config.json -dry-run
```

## Configuration

You can generate a configuration file with the `-generate-config` flag. This will create a skeleton
configuration with the default values.

You **must** populate `access_token`, `admin_room`, `homeserver`, and `listen_to` in the
configuration file in order to use the bot.

* `access_token`: The access token for the bot user.
* `admin_room`: The room ID of the admin room.
* `homeserver`: The homeserver URL (e.g. `https://matrix-client.matrix.org`).
* `listen_to`: A list of room IDs to listen to for `m.policy.rule.room` events.

Other options include:

* `log_level`: The log level to use. Can be `debug`, `info`, `warn`, or `error`. Defaults to `info`
* `legacy_version`: You **must enable this if you are using conduwuit v4.X.X** in order to properly
  evacuate and defederate rooms. 0.5.0 and later does not require this option. Defaults to `false`.

### Example Configuration

```json
{
    "access_token": "DWatRqOmK6KWuX9WSeMe0zWc6XGdo6UZ",
    "homeserver": "https://c2s.matrix.example",
    "listen_to": [
        "!fTjMjIzNKEsFlUIiru:neko.dev",
        "!WuBtumawCeOGEieRrp:matrix.org"
    ],
    "admin_room": "!abc:matrix.example",
    "log_level": "trace",
    "legacy_version": true
}
```

This example configuration will log in to `matrix.example`, and listen to the CME and Matrix.org
COC ban list rooms. It will then send admin commands to `!abc:matrix.example` using the legacy command format.

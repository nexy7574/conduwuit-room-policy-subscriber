# conduwuit room policy subscriber

This is a simple tool that will sit in your conduwuit admin room and listen for
`m.policy.rule.room` events. When it receives one, it will send a command to the 
conduwuit admin room to ban the room, and optionally also ban federation with it.

## Installing

```bash
go install github.com/nexy7574/conduwuit-room-policy-subscriber
```

## Usage

```bash
Usage of ./conduwuit-room-policy-subscriber:
  -defederate
        Ban federation of the room after banning
  -dry-run
        Don't actually send any messages
  -room string
        The alias of the admin room
  -token string
        The access token to use for the connection
  -url string
        The URL of the homeserver to connect to (default "https://matrix-client.matrix.org")
```

# auto

## Running

```bash
go run .
```

If you are serving the static assets through Firebase Hosting, run your Go API locally on port 8080 and proxy Hosting traffic to it with the Firebase emulator:

```bash
# in one terminal

go run .

# in another terminal
firebase emulators:start --only hosting
```

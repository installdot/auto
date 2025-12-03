# auto

## Running

Start the self-contained Go web server (serves the UI and APIs from the same binary):

```bash
go run .
```

Expose it publicly with ngrok using the provided token:

```bash
ngrok config add-authtoken 2v4SAqwEgLo93uCp6Fl6Jj0HKfD_kU5dWXMqYDirSYza7BbF
ngrok http 8080
```

If you prefer to keep Firebase Hosting in front, run the Go API locally on port 8080 and proxy Hosting traffic to it with the Firebase emulator (then optionally tunnel that port with ngrok):

```bash
# terminal 1: Go app
go run .

# terminal 2: Firebase Hosting emulator forwarding to localhost:8080
firebase emulators:start --only hosting --host 0.0.0.0 --port 5000

# optional terminal 3: tunnel the emulator or the Go app directly
ngrok http 8080  # or: ngrok http 5000
```

### Resolving merge conflicts

If you pulled upstream changes and Git reports conflicts in `README.md` or `main.go`, resolve them locally before pushing:

```bash
# see what changed
git status

# open the files and clean up <<<<<<, ======, >>>>>> markers
$EDITOR README.md main.go

# format Go after resolving
gofmt -w main.go

git add README.md main.go
git commit -m "Resolve merge conflicts"
```

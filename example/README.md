# Example app

A small application to try provino with, end to end, on your machine. It plays every part an application around provino plays:

- **A bare-bones OpenID Connect provider**. Two accounts sign in without a password, Alice and Bob, and get a JWT for provino (audience `http://localhost:3000`, scope `ingest`). provino gets its own tokens with the client credentials grant, as client `provino` with any secret, and presents them to the app.
- **The authorizer** provino asks before storing a browser upload. It puts each account's images in a namespace named after it, and picks the variants from the profile the browser chose. The rules are hardcoded in [`provino.go`](provino.go): Alice can upload every format up to 20 MB, and Bob JPEG and PNG only, up to 2 MB.
- **The webhook endpoint** provino reports every job to. It applies each event once, in the order the events happened.
- **A web UI** at `http://localhost:3001`, which uploads straight to provino from the browser and follows each job. Its tabs show each image's variants, every request provino made to the authorizer with the app's answer, and every webhook delivery with its headers and body.

Data is stored in `example/data`.

## Running it

You need what provino itself needs (Go and libvips), plus Node.js and [pnpm](https://pnpm.io) 12.11 or later for the web UI. From the repository's root, run:

```bash
make example
```

It installs the web UI's dependencies, then uses [goreman](https://github.com/mattn/goreman) to start the two servers in the [`Procfile`](Procfile): the app, and provino with the example's configuration, [`provino.yaml`](provino.yaml). provino waits for the app to answer, since it fetches the app's key set when it starts. The first run takes a while, because building provino compiles its libvips bindings. Ctrl+C stops both.

Open [http://localhost:3001](http://localhost:3001), sign in, and upload a few images.

> Use `localhost` rather than `127.0.0.1`: the page's origin has to be the one provino allows in `server.corsAllowedOrigins`.

To start over, stop the example and delete everything it stored:

```bash
make example-reset
```

## Changing it

Each server is built as it starts, so to try a change, restart that server from another terminal while the example runs:

```bash
go tool goreman run restart provino
```

The other server is `app`, and restarting it also rebuilds the web UI in [`web`](web), which the app embeds.

## What to try

- Upload as Bob in a format other than JPEG or PNG, or a file over 2 MB, and provino refuses it with what the authorizer decided. The Authorizer tab shows the request and the refusal.
- Upload a small image with the Photo profile: provino never enlarges it, so the boxes it can't fill are listed as skipped, sharing one file.
- Stop the app while a job runs with `go tool goreman run stop app`, then start it again with `go tool goreman run start app`: provino retries the webhook, and the Webhooks tab shows the delivery, with the same webhook ID on every attempt.
- Every variant key ends with a random file ID, so a file never changes once written, and the app serves it as immutable.

# Build the admin panel first: it is embedded into the server binary, so the
# Go stage cannot start without it.
FROM node:24-alpine AS web

WORKDIR /src
# Copy the manifests alone so that `npm ci` is cached until dependencies
# actually change.
COPY web/package.json web/package-lock.json ./web/
RUN npm --prefix web ci

COPY web ./web
RUN npm --prefix web run build


FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Overwrite whatever web/dist the repository carried with the panel just built.
COPY --from=web /src/web/dist ./web/dist

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/burrowd ./cmd/burrowd


# Distroless: no shell, no package manager, nothing for a compromised tunnel to
# pivot into.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/burrowd /usr/local/bin/burrowd

# The token store is rewritten whenever the panel edits a token, so it must
# live on a writable volume.
VOLUME ["/data"]

# Control port and HTTP. The TCP tunnel pool is a wide range and is not listed
# here: run the container with host networking, or publish the range explicitly
# with `-p 20000-30000:20000-30000`.
EXPOSE 7000 8080

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/burrowd"]
CMD ["-tokens", "/data/tokens.json", "-http", "0.0.0.0:8080"]

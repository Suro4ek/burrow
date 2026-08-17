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


FROM golang:1.25-alpine AS build

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

# The final image has no shell, so the state directory has to be made here and
# copied in below with the ownership burrowd needs.
RUN mkdir -p /out/data


# Distroless: no shell, no package manager, nothing for a compromised tunnel to
# pivot into.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/burrowd /usr/local/bin/burrowd

# The token store is rewritten whenever the panel edits a token, so it must
# live on a writable volume. Docker seeds a fresh named volume from whatever
# the image has at this path, ownership included -- without this the volume
# would arrive owned by root and burrowd, running as nonroot, could not write
# its tokens file. 65532 is distroless's nonroot uid, spelled numerically
# because a name would have to be resolved at build time.
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME ["/data"]

# Control port and HTTP. The TCP tunnel pool is a wide range and is not listed
# here: run the container with host networking, or publish the range explicitly
# with `-p 20000-30000:20000-30000`.
EXPOSE 7000 8080

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/burrowd"]
CMD ["-tokens", "/data/tokens.json", "-http", "0.0.0.0:8080"]

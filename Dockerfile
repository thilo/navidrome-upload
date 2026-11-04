FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY static ./static
# The Uppy bundle the page loads, pinned by checksum instead of committed.
ADD --checksum=sha256:eaa89e9ef0352e9ad569b6cb5232f2bac35ad5433611fde8b183ca7049493c65 \
    https://cdn.jsdelivr.net/npm/uppy@6.0.6/dist/uppy.min.mjs static/uppy.min.mjs
ADD --checksum=sha256:eca17b3a6c95940f50ffac04e26122e22ff92df1d58b6a0c46f8e2f40414b892 \
    https://cdn.jsdelivr.net/npm/uppy@6.0.6/dist/uppy.min.css static/uppy.min.css
RUN go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /navidrome-upload .

FROM python:3.13-slim
RUN pip install --no-cache-dir beets==2.14.1
COPY beets.yaml /etc/beets/config.yaml
COPY --from=build /navidrome-upload /usr/local/bin/navidrome-upload
# Must be able to write the music files; override with compose's user:.
USER 1000:1000
ENTRYPOINT ["navidrome-upload"]

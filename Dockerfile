FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /radio ./cmd/radio

FROM alpine:3.20
RUN apk add --no-cache ffmpeg
COPY --from=build /radio /usr/local/bin/radio
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["radio", "-data", "/data"]

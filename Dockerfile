FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/whatsapp-mcp ./cmd/whatsapp-mcp

FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates
COPY --from=build /out/whatsapp-mcp /usr/local/bin/whatsapp-mcp
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/whatsapp-mcp"]

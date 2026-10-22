# Build:  docker build -t universal .
# Relay:  docker run -d --name relay -p 80:80 -p 443:443 -p 20000-20100:20000-20100 -v universal-certs:/data/certs \
#           universal relay --domain example.com --token SECRET --email you@example.com --tcp-ports 20000-20100
# Monitor: docker run -d -p 8787:8787 -v universal-monitor:/data/monitor \
#           universal monitor --listen 0.0.0.0:8787 --token SECRET
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/universal ./cmd/universal

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -h /data universal
COPY --from=build /out/universal /usr/local/bin/universal
WORKDIR /data
ENV UNIVERSAL_CERTS=/data/certs UNIVERSAL_MONITOR_DATA=/data/monitor
EXPOSE 80 443 8787
ENTRYPOINT ["universal"]
CMD ["--help"]

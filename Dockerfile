FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X buddy2api-go/internal/proxy.Version=$(cat VERSION 2>/dev/null || echo dev)" -o /buddy2api ./cmd/buddy2api

FROM alpine:3.20
RUN adduser -D -u 1000 buddy2api
COPY --from=build /buddy2api /usr/local/bin/buddy2api
USER buddy2api
EXPOSE 10082
VOLUME /app/data
WORKDIR /app
ENTRYPOINT ["buddy2api"]

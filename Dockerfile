FROM golang:1.25-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X buddy2api-go/internal/proxy.Version=${VERSION}" -o /buddy2api .

FROM alpine:3.20
RUN apk add --no-cache tzdata \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone \
    && adduser -D -u 1000 buddy2api
ENV TZ=Asia/Shanghai
COPY --from=build /buddy2api /usr/local/bin/buddy2api
USER buddy2api
EXPOSE 10082
VOLUME /app/data
WORKDIR /app
ENTRYPOINT ["buddy2api"]

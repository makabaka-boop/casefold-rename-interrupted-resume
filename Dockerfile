# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go vet ./... && go test ./... && go build -o /out/planner .

FROM alpine:3.20
RUN adduser -D -u 10001 planner
USER planner
COPY --from=build /out/planner /usr/local/bin/planner
ENTRYPOINT ["planner"]

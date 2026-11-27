FROM golang:1-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/server ./cmd/server
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /pickup ./cmd/server && mkdir /data

FROM scratch
COPY --from=build /pickup /pickup
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/pickup", "-addr=:8080", "-data=/data/stock.json"]

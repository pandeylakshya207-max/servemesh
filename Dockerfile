ARG GO_VERSION=1.27

FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mockbackend ./cmd/mockbackend

FROM gcr.io/distroless/static-debian12:nonroot AS gateway
COPY --from=build /out/gateway /gateway
EXPOSE 8080
ENTRYPOINT ["/gateway"]

FROM gcr.io/distroless/static-debian12:nonroot AS mockbackend
COPY --from=build /out/mockbackend /mockbackend
EXPOSE 9000
ENTRYPOINT ["/mockbackend"]

# syntax=docker/dockerfile:1

FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go vet ./... \
 && go test ./... \
 && CGO_ENABLED=0 go build -trimpath -o /out/txcheck .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/txcheck /usr/local/bin/txcheck
ENTRYPOINT ["/usr/local/bin/txcheck"]

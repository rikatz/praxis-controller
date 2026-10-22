FROM golang:1.27.2 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -o /out/controller ./cmd/controller

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]

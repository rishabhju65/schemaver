# Build.
FROM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first, so a source-only change does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binary: the runtime image has no libc to link against.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /schemaver ./cmd/schemaver

# Run.
#
# Templates and migrations are embedded in the binary, so the runtime image needs
# nothing but the binary itself — no shell, no package manager, and nothing for a
# compromised process to pivot to.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /schemaver /schemaver
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/schemaver"]
CMD ["run"]

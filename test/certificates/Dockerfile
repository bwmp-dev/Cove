FROM golang:1.26.11-alpine AS build
WORKDIR /src
COPY main.go ./main.go
RUN CGO_ENABLED=0 GO111MODULE=off go build -trimpath -o /provider main.go

FROM scratch
COPY --from=build /provider /provider
USER 10001:10001
ENTRYPOINT ["/provider"]

GOOS=linux GOARCH=arm64 go build -o builder-arm64 cmd/builder/main.go
GOOS=linux GOARCH=amd64 go build -o builder-amd64 cmd/builder/main.go


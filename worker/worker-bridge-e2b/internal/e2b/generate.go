package e2b

//go:generate protoc --go_out=. --go_opt=paths=source_relative --connect-go_out=. --connect-go_opt=paths=source_relative process/v1/process.proto

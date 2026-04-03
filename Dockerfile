FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o server .

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/server .
COPY config.yaml .
COPY certs/ ./certs/        
EXPOSE 8080 9090            
CMD ["./server"]

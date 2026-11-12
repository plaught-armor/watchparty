# Web app: Node is only needed at build time.
FROM node:24-alpine AS web
WORKDIR /src
COPY package.json package-lock.json ./
# Install scripts only build native modules used by the old Node server.
RUN npm ci --no-audit --no-fund --ignore-scripts
COPY . .
# An empty Firebase config hides the sign-in UI; this server has no accounts.
ENV VITE_FIREBASE_CONFIG=""
RUN npx vite build

FROM golang:1.27-alpine AS server
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /watchparty .

FROM alpine:3.22
RUN adduser -D -H app
WORKDIR /app
COPY --from=server /watchparty ./watchparty
COPY --from=web /src/build ./build
COPY words ./words
USER app
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app/watchparty"]

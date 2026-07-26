FROM node:18-alpine

WORKDIR /app

# Copy package files and install dependencies
COPY package*.json ./
RUN npm ci --only=production

# Copy application code
COPY *.js ./
COPY config.yaml ./
COPY views/ ./views/
COPY public/ ./public/

# Create necessary directories
RUN mkdir -p logs data

# Expose the application port
EXPOSE 3000

# Run the app with a non-root user
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
RUN chown -R appuser:appgroup /app
USER appuser

CMD ["node", "index.js"] 
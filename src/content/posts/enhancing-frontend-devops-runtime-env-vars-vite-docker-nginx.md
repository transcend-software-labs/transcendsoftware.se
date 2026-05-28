---
title: "Managing Runtime env variables in JS Vite applications with Docker and Nginx"
description: "How to manage runtime environment variables in a Vite application using a single Docker image across all environments, injecting variables dynamically via Nginx and an entrypoint script."
date: 2024-01-19
tags: ["JavaScript", "JS", "Vite", "Docker", "Environment variables", "Runtime environment variables", "React-router-dom"]
---

## Introduction

In modern web development, managing environment configurations across different stages of deployment can be a challenging task. This post dives into an effective solution using Javascript and Vite, Docker and Nginx. By the end of this post, you will understand how to manage runtime environment variables effectively, ensuring smoother transitions from development to production.

## The challenge

Traditionally, handling different configurations for different environments requires creating separate Docker images for each scenario, which is inefficient and error-prone. This approach increases the risk of discrepancies between environments, leading to the dreaded "it works on my machine" syndrome.

## The solution

The strategy focuses on using a single Docker image across all environments, injecting runtime variables dynamically. This ensures consistency and reliability in deployments, and here is how to do it:

## Code Breakdown

**Dockerfile Configuration:**

The Dockerfile is set up to use Nginx and to copy the built Vite application into the Nginx server:

```dockerfile
FROM nginx:alpine
COPY dist /usr/share/nginx/html
COPY default.conf /etc/nginx/conf.d/default.conf
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 80
ENTRYPOINT ["/entrypoint.sh"]
CMD ["nginx", "-g", "daemon off;"]
```

**Nginx Configuration (default.conf):**

This configuration ensures that all requests are redirected to `index.html`, facilitating SPA routing:

```nginx
server {
    listen 80;
    location / {
        root   /usr/share/nginx/html;
        index  index.html index.htm;
        try_files $uri /index.html;
    }
}
```

**Entrypoint Script (entrypoint.sh):**

It replaces placeholders in `index.html` with actual environment variables at container startup:

```sh
#!/bin/sh
sed -i "s|__BACKEND_URL__|${BACKEND_URL}|g" /usr/share/nginx/html/index.html
sed -i "s|__SERVICE_VERSION__|${SERVICE_VERSION}|g" /usr/share/nginx/html/index.html
exec "$@"
```

**Frontend Setup (index.html):**

Initialize global variables with local defaults. The entrypoint script replaces the placeholder strings when running outside localhost:

```html
<script>
  globalThis.appConfig = {
    BACKEND_URL: "http://your-default-backend-url",
    SERVICE_VERSION: "local",
  };
  if (
    window.location.hostname !== "localhost" &&
    window.location.hostname !== "127.0.0.1"
  ) {
    globalThis.appConfig.BACKEND_URL = "__BACKEND_URL__";
    globalThis.appConfig.SERVICE_VERSION = "__SERVICE_VERSION__";
  }
</script>
```

The variables are available throughout the application on `globalThis.appConfig`.

## Security Considerations

Be cautious with environment variables. Avoid embedding sensitive data directly into your frontend code. Use backend services to handle sensitive operations.

## Conclusion

This setup streamlines the process of managing runtime environment variables in a frontend application, making deployments consistent and reliable. A single Docker image, runtime injection via entrypoint, and a simple Nginx configuration is all it takes.

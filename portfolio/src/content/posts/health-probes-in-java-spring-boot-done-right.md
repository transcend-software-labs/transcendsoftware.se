---
title: "Health probes in Java Spring Boot done right"
description: "How to set up health probes in Java using Spring Boot the right way"
date: 2023-04-04
tags: ["Java", "Spring Boot", "Kubernetes", "Health probes", "Spring Boot Actuator", "Monitoring"]
featured: true
---

## Health probes

Health probes are used by Kubernetes to monitor your services health. There is different kinds of probes that can be set up.

### Liveness probe

Used for detecting applications in broken states. Kubernetes will restart applications that reports negatively on liveness.

### Readiness probe

Used for detecting applications which is not able to serve traffic temporarily. Kubernetes won't kill applications that reports negatively on readiness, but it will not send traffic to it until it's ready again.

### Startup probe

Used for detecting if the application is ready to serve traffic. This probe is only used after starting up an application, and will not be used after it's started and the startup probe has reported positively.

To read more in detail about the probes, I refer to the [Kubernetes documentation](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/).

## Serving the probes

Kubernetes has different ways of configuring how the probes should query the application. A common way is using HTTP by making a HTTP request towards the application which responds with the status.

## Spring Boot Actuator

Provides endpoints which serves the health probes out of the box. Those endpoints will use Spring's built in availability functionality and read from it when responding on the endpoints. I suggest to use this functionality rather than implementing your own.

Apart from providing the above functionality you can also serve these endpoints on a separate port. Imagine that your service is overloaded and you have no threads able to serve requests. Kubernetes is trying to query your liveness endpoint but it does not respond in time, so it proceeds to restart your service. You don't want that. By serving these requests on a separate port, they are not affected by the regular traffic that's reaching your service. This is because these requests will be served by a different threadpool.

This can be configured using application properties:

```yaml
management:
  server:
    port: 9090
  endpoint:
    health:
      probes:
        enabled: true
  health:
    livenessState:
      enabled: true
    readinessState:
      enabled: true
  endpoints:
    web:
      exposure:
        include: health
```

You can of course change the port to whatever suits your needs. The rest of the configuration just exposes the health endpoints, while making sure no other endpoints is exposed by Spring Boot Actuator.

The liveness endpoint will be exposed at `/actuator/health/liveness` and the readiness at `/actuator/health/readiness`.

## Kubernetes probe definitions

This is an example of a liveness probe definition in Kubernetes:

```yaml
apiVersion: v1
kind: Pod
metadata:
  labels:
    test: liveness
  name: liveness-http
spec:
  containers:
    - name: liveness
      image: registry.k8s.io/liveness
      livenessProbe:
        httpGet:
          path: /actuator/health/liveness
          port: 9090
        initialDelaySeconds: 3
        periodSeconds: 3
```

It calls the liveness endpoint exposed by Spring Boot Actuator on port 9090 that was defined with application properties above. Those http requests will now be served on a separate threadpool than the regular requests reaching the service.

## Modifying liveness and readiness state

As touched upon above Spring Boot provides an easy way to modify the liveness and readiness states. Here's an example on how you could do that:

```java
@ControllerAdvice
public class ApplicationExceptionHandler {
    private final ApplicationEventPublisher eventPublisher;

    public ApplicationExceptionHandler(ApplicationEventPublisher eventPublisher) {
        this.eventPublisher = eventPublisher;
    }

    @ExceptionHandler(CacheIsCompletelyBrokenException.class)
    public void handleCacheIsCompletelyBrokenException(CacheIsCompletelyBrokenException ex) {
        AvailabilityChangeEvent.publish(this.eventPublisher, ex, LivenessState.BROKEN);
        throw ex;
    }

    @ExceptionHandler(ApplicationIsOverwhelmedException.class)
    public void handleApplicationIsOverwhelmedException(ApplicationIsOverwhelmedException ex) {
        AvailabilityChangeEvent.publish(this.eventPublisher, ex, ReadinessState.REFUSING_TRAFFIC);
        throw ex;
    }
}
```

If the application recovers, you can change the state back again:

```java
AvailabilityChangeEvent.publish(this.eventPublisher, "Application available for work again", ReadinessState.ACCEPTING_TRAFFIC);
```

Which will in turn make the `/actuator/health/readiness` endpoint report positively again.

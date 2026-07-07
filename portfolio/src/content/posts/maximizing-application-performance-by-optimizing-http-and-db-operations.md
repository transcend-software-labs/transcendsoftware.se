---
title: "Maximizing application performance by optimizing HTTP and Database operations"
description: "Understanding why reducing and optimizing HTTP and Database calls is the most efficient way to improve application performance, with practical tips and examples."
date: 2024-07-26
featured: true
tags: ["Performance", "HTTP", "Database", "DB", "Software architecture", "Multithreading", "Async processing", "Indexing", "Java"]
---

## Introduction

In modern software architecture, optimizing performance is crucial for building efficient and scalable applications. This post showcases the importance of reducing and optimizing HTTP and DB calls as the most efficient way to improve performance. We'll provide practical tips and examples to help you achieve significant performance gains.

## Understanding Operation Costs

To understand why reducing HTTP and DB calls is so effective, let's look at the relative costs of various operations.

```
Latency Comparison Numbers (~2012)
----------------------------------
L1 cache reference                           0.5 ns
Branch mispredict                            5   ns
L2 cache reference                           7   ns                      14x L1 cache
Mutex lock/unlock                           25   ns
Main memory reference                      100   ns                      20x L2 cache, 200x L1 cache
Compress 1K bytes with Zippy             3,000   ns        3 us
Send 1K bytes over 1 Gbps network       10,000   ns       10 us
Read 4K randomly from SSD*             150,000   ns      150 us          ~1GB/sec SSD
Read 1 MB sequentially from memory     250,000   ns      250 us
Round trip within same datacenter      500,000   ns      500 us
Read 1 MB sequentially from SSD*     1,000,000   ns    1,000 us    1 ms  ~1GB/sec SSD, 4X memory
Disk seek                           10,000,000   ns   10,000 us   10 ms  20x datacenter roundtrip
Read 1 MB sequentially from disk    20,000,000   ns   20,000 us   20 ms  80x memory, 20X SSD
Send packet CA->Netherlands->CA    150,000,000   ns  150,000 us  150 ms
```

The illustration above highlights that network calls, such as HTTP and DB operations, are the most expensive operations in terms of latency. Sending packets over a network can be tens to hundreds of thousands of times slower than accessing memory or even disk operations.

### Example: List vs. Map Lookup

Consider a simple example of searching for an element in a list versus a map. Looping through a list with 1,000 elements to find one might take around 1 millisecond. Using a map to find an element by its key is nearly instantaneous (constant time lookup):

```java
// List search example
List<String> list = Arrays.asList("a", "b", "c", ...);
String target = "z";
for (String s : list) {
    if (s.equals(target)) {
        break;
    }
}

// Map search example
Map<String, String> map = new HashMap<>();
map.put("a", "value1");
map.put("b", "value2");
...
String value = map.get("z");
```

While optimizing such code can save a couple of milliseconds, reducing HTTP and DB calls can save hundreds of milliseconds, especially at scale.

## Debunking Distributed Caching

It's often recommended to use a distributed cache to improve performance. However, communicating with a distributed cache typically involves network calls, which are expensive in terms of latency. A distributed cache is just another database, optimized for caching. Here's why caching in application memory is often a better approach:

- **Latency:** Accessing a distributed cache involves network overhead, similar to other HTTP calls. In contrast, in-memory caching is nearly instantaneous, with nanosecond-level access times.
- **Complexity:** Distributed caches add complexity to your architecture, requiring additional maintenance and potential handling of cache consistency issues.
- **Cost:** Operating and scaling distributed caches can be more costly compared to leveraging available in-memory resources.

This is why I prefer to have an optimized database called in case of a cache miss, and just cache in memory. With a well-optimized database, the performance difference between calling the database and calling a distributed cache should be negligible in many cases.

### Example: In-Memory Caching vs. Distributed Caching

```java
// In-memory caching example using Caffeine
Cache<String, Data> cache = Caffeine.newBuilder()
    .expireAfterWrite(10, TimeUnit.MINUTES)
    .maximumSize(10_000)
    .build();

public Data getCachedData(String key) {
    return cache.get(key, k -> fetchDataFromDB(k));
}

// Distributed caching with Redis example. Performs network calls under the hood.
public Data getCachedDataDistributed(String key) {
    try (Jedis jedis = jedisPool.getResource()) {
        String data = jedis.get(key);
        if (data == null) {
            Data freshData = fetchDataFromDB(key);
            jedis.set(key, serialize(freshData));
            return freshData;
        }
        return deserialize(data);
    }
}
```

In this example, accessing data from an in-memory cache is much faster than fetching from a distributed cache due to the absence of network latency.

## Optimizing Performance

### Reduce HTTP Calls

Reducing the number of HTTP calls is one of the most effective ways to improve performance:

- **Batching:** Combine multiple small requests into a single batch request.
- **Caching:** Store frequently accessed data temporarily to reduce repeated calls.
- **Pooling:** Reuse existing connections to avoid the overhead of establishing new ones (which can take hundreds of milliseconds).

### Reduce DB Calls

Similarly, reducing DB calls can lead to significant performance improvements:

- **Batching:** Execute multiple operations in a single query.
- **Caching:** Temporarily store frequently accessed data.
- **Pooling:** Reuse DB connections to minimize connection overhead.
- **Return Data on Insert/Update:** Return data in update/insert queries to remove the need for a subsequent query.

### Parallel Execution

Execute DB queries and HTTP calls in parallel using different threads to make the most of available resources:

```java
var executor = Executors.newVirtualThreadPerTaskExecutor();
Future<Response> future1 = executor.submit(() -> httpClient.execute(request1));
Future<Response> future2 = executor.submit(() -> httpClient.execute(request2));
```

### Async Processing

Perform expensive tasks asynchronously if they don't need to be completed within the request-response cycle. For example, audit logging or session tracking can be processed asynchronously.

```java
CompletableFuture.runAsync(() -> performAuditLogging());
```

### Optimize DB Queries

- **Indexing:** Use indexes to speed up data retrieval.
- **Selective Fields:** Select only the necessary fields to reduce data transfer.
- **Explain Plan:** Use the explain plan to check which indexes are used and how queries will perform.

```sql
EXPLAIN ANALYZE SELECT id, name FROM users WHERE email = 'example@example.com';
```

- **Direct Queries:** Write your queries directly instead of relying on ORMs for better control and performance.

## Why Milliseconds Matter at Scale

At scale, even small inefficiencies can have a significant impact. A few too many milliseconds of processing time can be the make or break for being able to handle all incoming traffic. If anything lags behind, requests will stack up waiting for processing and cause major issues. Here's why those milliseconds matter:

- **High Traffic:** In a high-traffic application, handling thousands of requests per second, each additional millisecond of latency can result in substantial delays and increased load on your servers.
- **Request Stacking:** When requests take longer to process, they can stack up, leading to longer response times, increased memory usage, and potentially causing timeouts or failures.
- **User Experience:** Performance bottlenecks directly impact user experience. Faster response times lead to higher user satisfaction and better retention.

## Conclusion

Reducing and optimizing HTTP and DB calls is by far the most efficient way to improve application performance. By focusing on these areas first, you can achieve significant gains. Remember to also optimize code and queries for further improvements. Implementing these strategies will help you build scalable and high-performing applications.

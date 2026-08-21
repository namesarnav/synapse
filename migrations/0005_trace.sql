-- W3C trace context of the request that created the execution, so worker spans join the same trace.
ALTER TABLE executions ADD COLUMN traceparent TEXT NOT NULL DEFAULT '';

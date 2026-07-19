-- Privacy-friendly acquisition metrics. Rows are aggregate daily counters only:
-- no IP address, user agent, cookie id, email, or other visitor identifier is
-- stored. Campaign fields come from explicit UTM parameters on the public URL.
CREATE TABLE IF NOT EXISTS marketing_daily (
    day        DATE   NOT NULL,
    event      TEXT   NOT NULL,
    source     TEXT   NOT NULL DEFAULT '',
    medium     TEXT   NOT NULL DEFAULT '',
    campaign   TEXT   NOT NULL DEFAULT '',
    count      BIGINT NOT NULL DEFAULT 0 CHECK (count >= 0),
    PRIMARY KEY (day, event, source, medium, campaign)
);

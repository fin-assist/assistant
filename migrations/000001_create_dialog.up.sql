-- Dialogue of the LLM assistant (FIN-114): the user's state (time zone, daily question limit),
-- questions, answers and answer blocks. Design, expected queries and alternatives — docs/schema.md.
--
-- The schema keeps keys, foreign keys with cascades, NOT NULL and DEFAULT only: no CHECK,
-- triggers or functions. Value and state rules (status transitions, error fields, block form,
-- lengths) are checked by the service code (docs/schema.md, «Инварианты в коде сервиса»).
--
-- Every table has user_id first in its primary key: every query is scoped by the user, and the
-- data of one user stays together (docs «Хранение данных»). Rows of a user hang off user_state
-- through ON DELETE CASCADE, so PurgeUserData is one DELETE of that row.

-- One row per user: the root of the user's data, the per-user lock of writing transactions,
-- the time zone from SetUserTimezone and the counter of today's questions.
CREATE TABLE user_state (
    user_id          uuid        NOT NULL,
    -- IANA zone and settings_version from usersettings.v1.SetUserTimezone; NULL until the first
    -- delivery, then the service answers with Europe/Moscow (grpc-api.md 7.4).
    time_zone        text,
    settings_version bigint,
    -- Day (Europe/Moscow) the counter belongs to and questions charged on that day. A new day
    -- starts the counter from 1; a refund for a past day changes nothing.
    limit_day        date,
    -- Integer width by the number of values («Архитектура» → «Идентификаторы»): a few per day.
    -- squawk-ignore prefer-bigint-over-smallint
    limit_used       smallint    NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_state_pkey PRIMARY KEY (user_id)
);

-- A question of the user (role user in REST 7.3). id is the message id of the API.
CREATE TABLE question (
    user_id       uuid        NOT NULL,
    -- UUIDv7 made by the database inside the transaction that holds the user_state lock:
    -- the order of id is the order of the user's questions, the key of the history pages.
    id            uuid        NOT NULL DEFAULT uuidv7(),
    text          text        NOT NULL,
    -- Mode of transfers the answer is computed in (REST 7.1): with, without.
    transfer_mode text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT question_pkey PRIMARY KEY (user_id, id),
    CONSTRAINT question_user_fkey FOREIGN KEY (user_id)
        REFERENCES user_state (user_id) ON DELETE CASCADE
);

-- An answer of the assistant (role assistant in REST 7.3). id is answer_id of the API.
-- A question has one active answer; a retry (REST 7.4) marks the failed one replaced and adds
-- a new one, so a replaced answer stays only for its stream to answer NOT_FOUND.
CREATE TABLE answer (
    user_id       uuid        NOT NULL,
    id            uuid        NOT NULL DEFAULT uuidv7(),
    question_id   uuid        NOT NULL,
    -- generating -> complete | failed; nothing leaves complete or failed.
    status        text        NOT NULL DEFAULT 'generating',
    -- Whether the attempt counts against the limit now: true while generating, false after a
    -- refund (any failure, or a complete answer with missing data), REST 7.2 «charged».
    charged       boolean     NOT NULL DEFAULT true,
    -- user_state.limit_day the attempt was charged to: a refund decrements that day only.
    charge_day    date        NOT NULL,
    -- Only for failed: open code of REST 7.2 «error» (MODEL_ERROR, CANNOT_ANSWER, OFF_TOPIC, ...),
    -- the text shown to the user and whether RetryAnswer is allowed (FIN-159).
    error_code    text,
    error_text    text,
    retryable     boolean,
    -- Currency of the amounts in the blocks (ISO 4217), RUB for now (FIN-82).
    currency      text        NOT NULL DEFAULT 'RUB',
    -- Source of the answer (REST 7.2 «source»): when the data was calculated and the period it
    -- covers, half-open [data_from, data_to). changed_since is computed on read. transfer_mode
    -- is the question's.
    calculated_at timestamptz,
    data_from     timestamptz,
    data_to       timestamptz,
    -- Navigation to the data (REST 7.2 «chip»): a JSON array, in the order they were sent.
    chips         jsonb       NOT NULL DEFAULT '[]',
    created_at    timestamptz NOT NULL DEFAULT now(),
    finished_at   timestamptz,
    replaced_at   timestamptz,
    CONSTRAINT answer_pkey PRIMARY KEY (user_id, id),
    CONSTRAINT answer_question_fkey FOREIGN KEY (user_id, question_id)
        REFERENCES question (user_id, id) ON DELETE CASCADE
);

-- At most one active (not replaced) answer per question: the invariant of REST 7.4.
CREATE UNIQUE INDEX answer_active_key ON answer (user_id, question_id) WHERE replaced_at IS NULL;

-- Every answer of a question, replaced ones included: the cascade from question and
-- PurgeUserData look answers up by question.
CREATE INDEX answer_question_idx ON answer (user_id, question_id);

-- Answers still generating, oldest first: the job that fails generations older than
-- 3 minutes (a pod died mid-answer, REST 7.2) reads only these rows.
CREATE INDEX answer_generating_idx ON answer (created_at) WHERE status = 'generating';

-- A block of an answer at position idx (0, 1, 2, ...), in the shared catalog form of
-- api.md «Блоки»: type and schema version v as columns, the rest of the block in body.
-- A text block keeps its Markdown in text, which grows by chunks during generation; it has
-- no body. The stream reads a block by (user_id, answer_id, idx) after a notification.
CREATE TABLE answer_block (
    user_id   uuid     NOT NULL,
    answer_id uuid     NOT NULL,
    -- Position and schema version: tens of values at most (int32 in the contract).
    -- squawk-ignore prefer-bigint-over-smallint
    idx       smallint NOT NULL,
    type      text     NOT NULL,
    -- squawk-ignore prefer-bigint-over-smallint
    v         smallint NOT NULL,
    text      text,
    -- Block envelope without type and v: title, alt_text, data (catalog schema type@v).
    body      jsonb,
    CONSTRAINT answer_block_pkey PRIMARY KEY (user_id, answer_id, idx),
    CONSTRAINT answer_block_answer_fkey FOREIGN KEY (user_id, answer_id)
        REFERENCES answer (user_id, id) ON DELETE CASCADE
);

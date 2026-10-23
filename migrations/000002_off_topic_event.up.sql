-- FIN-161: the resettable ledger is separate from the retained dialogue history.
CREATE TABLE off_topic_event (
    user_id uuid NOT NULL,
    answer_id uuid NOT NULL,
    completed_at timestamptz NOT NULL,
    CONSTRAINT off_topic_event_pkey PRIMARY KEY (user_id, answer_id),
    CONSTRAINT off_topic_event_answer_fkey FOREIGN KEY (user_id, answer_id)
        REFERENCES answer (user_id, id) ON DELETE CASCADE
);

COMMENT ON TABLE off_topic_event IS
    'Учитываемые исходы OFF_TOPIC после последнего успешного complete charged=true. Сброс удаляет записи, история ответов сохраняется.';
COMMENT ON COLUMN off_topic_event.user_id IS 'Пользователь, которому принадлежит терминальный ответ.';
COMMENT ON COLUMN off_topic_event.answer_id IS 'Ответ с исходом OFF_TOPIC; один ответ учитывается ровно один раз.';
COMMENT ON COLUMN off_topic_event.completed_at IS 'Время терминального перехода: случай учитывается строго до completed_at + настроенное окно.';

CREATE INDEX off_topic_event_completed_idx ON off_topic_event (user_id, completed_at);

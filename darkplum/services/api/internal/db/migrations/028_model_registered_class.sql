-- The strictest backend class that has ever served a model id, kept apart
-- from models.backend_class on purpose.
--
-- models.backend_class is overwritten on every sync with the class of
-- whichever backend serves the id RIGHT NOW. When a self-hosted backend goes
-- down and a vendor API that lists the same id (an OpenAI-compatible vendor
-- serving the same Hugging Face id) takes the id over, the next sync would
-- rewrite backend_class to external_api and the gateway would have nothing
-- left to tell it that the id normally runs in-cluster. This table is what it
-- compares the serving class against: a model id is never served by a looser
-- class than its registered one (gateway: 503 model_class_downgraded).
--
-- The sync only ever TIGHTENS a row (a stricter class that claims the id
-- replaces a looser one; a looser claim never replaces a stricter one). The
-- only way to loosen it is the admin action PATCH /api/v1/models/:id
-- {"accept_class": true}, which pins it to the class the model is discovered
-- under now and is audited (model.class_accepted).
--
-- Not purged with the models row (7 days unseen): a long outage must not
-- forget what the id used to be.
CREATE TABLE IF NOT EXISTS model_registered_classes (
    model_id         TEXT PRIMARY KEY,
    registered_class TEXT NOT NULL
        CHECK (registered_class IN ('self_hosted', 'cloud_managed', 'external_api')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Existing models are registered at the STRICTEST of the class they carry today
-- and every class the usage log (inference_events.backend_class, written per
-- request for the RESOLVED model id) ever recorded for them. A model id that a
-- vendor had already taken over before this migration runs is therefore
-- registered at its earlier, stricter class when history shows it. With no
-- history (or history only at the looser class) the id is registered at what
-- it carries now; compare this table with your self-hosted inventory after
-- upgrading and fix a wrong row with PATCH /api/v1/models/:id
-- {"accept_class": true} (see the runbook). Idempotent: re-running changes
-- nothing, because existing rows are never overwritten.
INSERT INTO model_registered_classes (model_id, registered_class)
SELECT c.model_id,
       CASE MIN(c.rank) WHEN 0 THEN 'self_hosted' WHEN 1 THEN 'cloud_managed' ELSE 'external_api' END
FROM (
    SELECT m.model_id,
           CASE m.backend_class WHEN 'self_hosted' THEN 0 WHEN 'cloud_managed' THEN 1 ELSE 2 END AS rank
    FROM models m
    WHERE m.backend_class IN ('self_hosted', 'cloud_managed', 'external_api')
  UNION ALL
    SELECT ie.model,
           CASE ie.backend_class WHEN 'self_hosted' THEN 0 WHEN 'cloud_managed' THEN 1 ELSE 2 END
    FROM inference_events ie
    WHERE ie.backend_class IN ('self_hosted', 'cloud_managed', 'external_api')
      AND ie.model IN (SELECT model_id FROM models)
) c
GROUP BY c.model_id
ON CONFLICT (model_id) DO NOTHING;

import { z } from "zod";

const ComponentSchema = z.object({
  kind: z.string(),
  version: z.string(),
  commit: z.string(),
});
const TargetSchema = z.object({
  artifact: z.object({
    id: z.string(),
    host: z.string(),
    kind: z.string(),
    version: z.string(),
    commit: z.string(),
    component_versions: z.record(z.string(), z.string()).optional(),
  }),
  baseline: z.object({ commit: z.string() }),
  phase: z.string(),
  operation: z.string(),
  attempt: z.number(),
  recovery: z
    .object({
      status: z.string(),
      candidate_error: z.string().optional(),
      candidate_stage: z.string().optional(),
      error: z.string().optional(),
    })
    .optional(),
  error: z.string().optional(),
  log: z.string().optional(),
  failure_stage: z.string().optional(),
  rollback_available: z.boolean(),
  package_ready: z.boolean().optional(),
  observation: z
    .object({ at: z.string(), components: z.array(ComponentSchema) })
    .optional(),
});
export const ForkUpdateStateSchema = z.object({
  revision: z.number().int().positive(),
  receipt: z.object({
    digest: z.string().regex(/^[0-9a-f]{64}$/),
    approved: z.boolean(),
    sequence: z.number(),
    targets: z.record(z.string(), TargetSchema),
  }),
});
export type ForkUpdateState = z.infer<typeof ForkUpdateStateSchema>;
export const ForkUpdateResponseSchema = z.union([
  ForkUpdateStateSchema,
  z.object({ available: z.literal(false) }),
]);
export type ForkUpdateResponse = z.infer<typeof ForkUpdateResponseSchema>;
export type ForkUpdateAction = "approve" | "retry" | "rollback";

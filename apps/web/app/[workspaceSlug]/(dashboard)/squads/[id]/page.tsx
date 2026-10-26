"use client";

import { use } from "react";
import { SquadDetailPage } from "@multica/views/squads";

export default function SquadDetailRoute({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params);
  return <SquadDetailPage squadId={id} />;
}

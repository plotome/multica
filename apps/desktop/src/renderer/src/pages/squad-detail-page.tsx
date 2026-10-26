import { useParams } from "react-router-dom";
import { SquadDetailPage as SharedSquadDetailPage } from "@multica/views/squads";

export function SquadDetailPage() {
  const { id } = useParams<{ id: string }>();
  return <SharedSquadDetailPage squadId={id ?? ""} />;
}

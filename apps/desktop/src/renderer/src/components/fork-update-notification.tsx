import {useEffect} from "react";
import {useQuery,useQueryClient} from "@tanstack/react-query";
import {api} from "@multica/core/api";
import {Button} from "@multica/ui/components/ui/button";
import {useT} from "@multica/views/i18n";
import {openSettingsTab} from "@/hooks/use-open-settings-shortcut";

export function ForkUpdateNotification({automaticChecks}:{automaticChecks:boolean}){
 const {t}=useT("settings");const client=useQueryClient();
 const query=useQuery({queryKey:["fork-update-state"],queryFn:()=>api.getForkUpdateState(automaticChecks),retry:false});
 useEffect(()=>window.updater.onForkCheck?.(()=>{void client.invalidateQueries({queryKey:["fork-update-state"]})}),[client]);
 const state=query.data;
 if(!state||!("receipt" in state)||Object.values(state.receipt.targets).length===0||Object.values(state.receipt.targets).every(target=>target.phase==="succeeded"))return null;
 const text=t(($)=>$.desktop.coordinated_updates,{returnObjects:true});
 return <aside aria-label={text.title} className="fixed bottom-4 right-4 z-50 w-80 rounded-lg border border-border bg-background p-4 shadow-lg"><p className="text-body font-medium">{text.title}</p><p className="mt-2 text-caption text-muted-foreground">{state.receipt.approved?text.incomplete:text.available}</p><Button className="mt-3" size="sm" onClick={openSettingsTab}>{text.open_updates}</Button></aside>;
}

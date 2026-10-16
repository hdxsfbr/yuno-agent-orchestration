import { useQuery } from "@tanstack/react-query";
import { listSkills } from "./api";

export const skillsKey = ["skills"] as const;

export const useSkills = () => useQuery({ queryKey: skillsKey, queryFn: listSkills });

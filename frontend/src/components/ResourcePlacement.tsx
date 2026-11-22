import { productionEnvironments } from "../lib/hierarchy";
import { Select } from "./Select";
import { useId } from "react";
import type { Project } from "../api/projects";
import { can } from "../lib/permissions";
import { inputClass } from "../lib/styles";
import { Field } from "./ui";

export function ResourcePlacement({ projects, value, onChange }: { projects: Project[]; value: string; onChange: (value: string) => void }) {
  const id = useId();
  return (
    <Field id={id} label="Project and environment" hint="A project is required. Choose Project resources for resources shared across environments. Network connections are configured separately.">
      <Select id={id} className={inputClass} value={value} onChange={(event) => onChange(event.target.value)} required aria-describedby={`${id}-hint`}>
        <option value="">Select a project or environment</option>
        {projects.filter((project) => can(project.permissions, "edit") || project.environments.some((environment) => can(environment.permissions, "edit"))).map((project) => (
          <optgroup key={project.id} label={project.name}>
            {can(project.permissions, "edit") && <option value={`${project.id}:`}>{project.name} / Project resources</option>}
            {productionEnvironments(project).filter((environment) => can(environment.permissions, "edit")).map((environment) => (
              <option key={environment.id} value={`${project.id}:${environment.id}`}>{project.name} / {environment.name}</option>
            ))}
          </optgroup>
        ))}
      </Select>
    </Field>
  );
}

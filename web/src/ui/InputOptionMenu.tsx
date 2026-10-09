import {Check} from "lucide-react";

interface Props {
  value: string;
  options: readonly string[];
  disabled?: boolean;
  onChange: (value: string) => void;
}

// Input options are model-provided suggestions, not a compact settings menu.
// Keep the choices visible so the user can compare them before deciding.
export function InputOptionMenu({
  value,
  options,
  disabled,
  onChange
}: Props) {
  return (
    <section className="inputOptionRoot" aria-label="Suggested answers">
      <p className="inputOptionHeading">Suggested answers</p>
      <div className="inputOptionList" role="group" aria-label="Suggested answers">
        {options.map((option) => {
          const selected = option === value;
          return (
            <button
              className="inputOptionCard"
              key={option}
              type="button"
              aria-pressed={selected}
              disabled={disabled}
              data-selected={selected || undefined}
              title={option}
              onClick={() => onChange(option)}
            >
              <span className="inputOptionText">
                {option}
              </span>
              {selected && <Check className="inputOptionCheck" size={16} aria-hidden="true" />}
            </button>
          );
        })}
      </div>
    </section>
  );
}

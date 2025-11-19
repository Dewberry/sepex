"""
This script should be called from root of the repository.
"""

import os

import plantuml

output_md = "Sequence-Diagrams.md"

# Clear or create the markdown file
if os.path.exists(output_md):
    os.remove(output_md)

with open(output_md, "a") as md:
    md.write(
        """
Welcome to the sequence diagrams documentation. This page aims to explain the flow of activities for the sepex system. The source code of all UML Diagrams is located at `imgs/umls/sequence-diagrams`.

https://plantuml.com/ is used to generate PNGs.

"""
    )

for root, dirs, files in os.walk("imgs/umls/sequence-diagrams/"):
    for file in files:
        if file.endswith(".puml"):
            path = os.path.join(root, file)
            with open(path, "r") as uml_f:
                uml = uml_f.read()

            # Generate URL using plantuml library
            url = plantuml.PlantUML(url="https://www.plantuml.com/plantuml/png/").get_url(uml)

            # Extract filename without extension for the heading
            filename_no_ext = os.path.splitext(file)[0]
            with open(output_md, "a") as md:
                md.write(f"## {filename_no_ext}\n")
                md.write(f'![{file}]({url} "{file}")\n\n')
